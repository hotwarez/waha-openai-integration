package main

import (
	"bytes"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/tidwall/gjson"
)

type ImportRequest struct {
	Days int `json:"days"`
}

func startChatwootImport(client Client, global GlobalSetting, days int) {
	log.Printf("[Importer] Starting history import for client %s (%d days)", client.Name, days)

	wahaURL := strings.TrimRight(global.WahaURL, "/")
	wahaToken := global.WahaToken
	sessionName := client.WahaSession
	cwURL := strings.TrimRight(global.ChatwootURL, "/")
	cwToken := global.ChatwootToken
	accountID := client.ChatwootAccountID
	inboxID := client.ChatwootInboxID

	if cwURL == "" || cwToken == "" || accountID == 0 || inboxID == 0 {
		log.Printf("[Importer] Chatwoot not fully configured for client %s", client.Name)
		return
	}

	cutoffTime := time.Now().AddDate(0, 0, -days).Unix()

	req := resty.New().R()
	if wahaToken != "" {
		req.SetHeader("X-Api-Key", wahaToken)
		req.SetHeader("Authorization", "Bearer "+wahaToken)
	}

	// --- PROTECTION: Disable Chatwoot Webhooks during import to prevent WhatsApp bans ---
	var storedWebhooks []gjson.Result
	whResp, err := resty.New().R().
		SetHeader("api_access_token", cwToken).
		Get(fmt.Sprintf("%s/api/v1/accounts/%d/webhooks", cwURL, accountID))
	
	if err == nil && !whResp.IsError() {
		storedWebhooks = gjson.GetBytes(whResp.Body(), "payload").Array()
		for _, wh := range storedWebhooks {
			whID := wh.Get("id").Int()
			log.Printf("[Importer] Temporarily deleting Chatwoot webhook %d to prevent ban", whID)
			resty.New().R().
				SetHeader("api_access_token", cwToken).
				Delete(fmt.Sprintf("%s/api/v1/accounts/%d/webhooks/%d", cwURL, accountID, whID))
		}
	}

	defer func() {
		// Recreate webhooks
		for _, wh := range storedWebhooks {
			url := wh.Get("url").String()
			var subs []string
			for _, sub := range wh.Get("subscriptions").Array() {
				subs = append(subs, sub.String())
			}
			log.Printf("[Importer] Recreating Chatwoot webhook: %s", url)
			resty.New().R().
				SetHeader("api_access_token", cwToken).
				SetBody(map[string]interface{}{
					"webhook": map[string]interface{}{
						"url": url,
						"subscriptions": subs,
					},
				}).
				Post(fmt.Sprintf("%s/api/v1/accounts/%d/webhooks", cwURL, accountID))
		}
	}()
	// ----------------------------------------------------------------------------------

	// 1. Get Chats
	resp, err = req.Get(wahaURL + "/api/" + sessionName + "/chats")
	if err != nil || resp.IsError() {
		log.Printf("[Importer] Error fetching chats: %v", err)
		return
	}

	chats := gjson.ParseBytes(resp.Body()).Array()
	log.Printf("[Importer] Found %d chats", len(chats))

	for _, chat := range chats {
		chatId := chat.Get("id").String()
		if strings.HasSuffix(chatId, "@g.us") || strings.Contains(chatId, "-") {
			continue // Skip groups
		}
		
		// Optimization: Check chat's last message timestamp before fetching all its messages
		chatTs := chat.Get("timestamp").Int()
		if chatTs > 0 && chatTs < cutoffTime {
			continue // Skip this chat entirely, no recent messages
		}
		
		contactName := chat.Get("name").String()
		if contactName == "" {
			contactName = strings.Split(chatId, "@")[0]
		}
		phoneNumber := "+" + strings.Split(chatId, "@")[0]

		log.Printf("[Importer] Processing chat %s", phoneNumber)

		// 2. Get Messages for this chat
		msgResp, err := req.Get(fmt.Sprintf("%s/api/%s/chats/%s/messages?limit=150", wahaURL, sessionName, chatId))
		if err != nil || msgResp.IsError() {
			log.Printf("[Importer] Failed to fetch messages for %s", chatId)
			continue
		}

		messages := gjson.GetBytes(msgResp.Body(), "data").Array()
		if len(messages) == 0 {
			messages = gjson.ParseBytes(msgResp.Body()).Array()
		}
		var messagesToImport []gjson.Result

		for _, msg := range messages {
			ts := msg.Get("timestamp").Int()
			if ts >= cutoffTime {
				messagesToImport = append(messagesToImport, msg)
			}
		}

		if len(messagesToImport) == 0 {
			continue
		}

		log.Printf("[Importer] %d messages to import for %s", len(messagesToImport), phoneNumber)

		// 3. Find/Create Contact in Chatwoot
		cwClient := resty.New().R().
			SetHeader("api_access_token", cwToken).
			SetHeader("Content-Type", "application/json")

		searchResp, _ := cwClient.Get(fmt.Sprintf("%s/api/v1/accounts/%d/contacts/search?q=%s", cwURL, accountID, strings.Split(chatId, "@")[0]))
		
		contactID := int64(0)
		if searchResp != nil && !searchResp.IsError() {
			results := gjson.GetBytes(searchResp.Body(), "payload").Array()
			if len(results) > 0 {
				contactID = results[0].Get("id").Int()
			}
		}

		if contactID == 0 {
			// Create contact
			createResp, _ := cwClient.SetBody(map[string]interface{}{
				"name":         contactName,
				"phone_number": phoneNumber,
			}).Post(fmt.Sprintf("%s/api/v1/accounts/%d/contacts", cwURL, accountID))
			
			if createResp != nil && !createResp.IsError() {
				contactID = gjson.GetBytes(createResp.Body(), "payload.contact.id").Int()
			}
		}

		if contactID == 0 {
			log.Printf("[Importer] Failed to find/create contact %s", phoneNumber)
			continue
		}

		// 4. Create Conversation
		convResp, _ := cwClient.SetBody(map[string]interface{}{
			"source_id": phoneNumber,
			"inbox_id":  inboxID,
			"contact_id": contactID,
			"status": "resolved", // Keep it resolved to not clutter the inbox
		}).Post(fmt.Sprintf("%s/api/v1/accounts/%d/conversations", cwURL, accountID))

		convID := int64(0)
		if convResp != nil && !convResp.IsError() {
			convID = gjson.GetBytes(convResp.Body(), "id").Int()
		} else {
			// Search for existing conversation
			searchConvResp, _ := cwClient.Get(fmt.Sprintf("%s/api/v1/accounts/%d/contacts/%d/conversations", cwURL, accountID, contactID))
			if searchConvResp != nil && !searchConvResp.IsError() {
				convs := gjson.GetBytes(searchConvResp.Body(), "payload").Array()
				for _, c := range convs {
					if c.Get("inbox_id").Int() == int64(inboxID) {
						convID = c.Get("id").Int()
						break
					}
				}
			}
			
			if convID == 0 {
				log.Printf("[Importer] Error: Could not find or create conversation for %s", phoneNumber)
				continue
			}
		}

		// 5. Send Messages in chronological order (reverse the slice if needed)
		// Usually WAHA returns newest first? No, older WAHA might return oldest first. We should sort by timestamp.
		for i := len(messagesToImport) - 1; i >= 0; i-- {
			msg := messagesToImport[i]
			text := msg.Get("body").String()
			fromMe := msg.Get("fromMe").Bool()
			hasMedia := msg.Get("hasMedia").Bool()
			timestamp := msg.Get("timestamp").Int()

			msgType := 0 // incoming (from contact)
			if fromMe {
				msgType = 1 // outgoing (from agent)
			}
			
			if text == "" && !hasMedia {
				continue
			}

			// Prepend [Histórico: DD/MM/YYYY HH:MM] - 
			historyPrefix := fmt.Sprintf("[Histórico: %s] - ", time.Unix(timestamp, 0).Format("02/01/2006 15:04"))
			if text != "" {
				text = historyPrefix + text
			}
			
			if !hasMedia {
				cwClient.SetBody(map[string]interface{}{
					"content": text,
					"message_type": msgType,
					"private": false,
					"created_at": time.Unix(timestamp, 0).Format(time.RFC3339),
				}).Post(fmt.Sprintf("%s/api/v1/accounts/%d/conversations/%d/messages", cwURL, accountID, convID))
			} else {
				// Has media
				msgIdStr := msg.Get("id").String()
				
				// Attempt to download media from WAHA
				mediaResp, err := req.Get(wahaURL + "/api/" + sessionName + "/messages/" + msgIdStr + "/download")
				if err != nil || mediaResp.IsError() {
					// Fallback to text
					cwClient.SetBody(map[string]interface{}{
						"content": "📎 [Mídia não suportada na importação: " + text + "]",
						"message_type": msgType,
						"private": false,
						"created_at": time.Unix(timestamp, 0).Format(time.RFC3339),
					}).Post(fmt.Sprintf("%s/api/v1/accounts/%d/conversations/%d/messages", cwURL, accountID, convID))
					continue
				}

				// Upload to Chatwoot via multipart
				contentType := mediaResp.Header().Get("Content-Type")
				if contentType == "" {
					contentType = "application/octet-stream"
				}
				
				// Quick extension guess
				ext := ".bin"
				if strings.Contains(contentType, "image/jpeg") { ext = ".jpg" }
				if strings.Contains(contentType, "image/png") { ext = ".png" }
				if strings.Contains(contentType, "audio/ogg") { ext = ".ogg" }
				if strings.Contains(contentType, "video/mp4") { ext = ".mp4" }
				if strings.Contains(contentType, "application/pdf") { ext = ".pdf" }

				filename := "media_" + strconv.FormatInt(timestamp, 10) + ext

				// Send multipart
				if text == "" {
					text = historyPrefix + "Mídia"
				}
				
				_, err = resty.New().R().
					SetHeader("api_access_token", cwToken).
					SetMultipartFormData(map[string]string{
						"content": text,
						"message_type": strconv.Itoa(msgType),
						"private": "false",
					}).
					SetMultipartField("attachments[]", filename, contentType, bytes.NewReader(mediaResp.Body())).
					Post(fmt.Sprintf("%s/api/v1/accounts/%d/conversations/%d/messages", cwURL, accountID, convID))
					
				if err != nil {
					log.Printf("[Importer] Failed to upload media for %s: %v", msgIdStr, err)
				}
			}
			
			// Small delay to prevent rate limiting
			time.Sleep(1500 * time.Millisecond) // 1.5s delay to prevent rate limit & spam
		}
		
		log.Printf("[Importer] Completed chat %s", phoneNumber)
	}

	log.Printf("[Importer] Finished history import for client %s", client.Name)
}
