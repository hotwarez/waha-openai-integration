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

	// 1. Get Chats
	resp, err := req.Get(wahaURL + "/api/" + sessionName + "/chats")
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
		
		contactName := chat.Get("name").String()
		if contactName == "" {
			contactName = strings.Split(chatId, "@")[0]
		}
		phoneNumber := "+" + strings.Split(chatId, "@")[0]

		log.Printf("[Importer] Processing chat %s", phoneNumber)

		// 2. Get Messages for this chat
		msgResp, err := req.Get(fmt.Sprintf("%s/api/%s/chats/%s/messages?limit=1000", wahaURL, sessionName, chatId))
		if err != nil || msgResp.IsError() {
			log.Printf("[Importer] Failed to fetch messages for %s", chatId)
			continue
		}

		messages := gjson.ParseBytes(msgResp.Body()).Array()
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
			// Maybe it already exists, let's search it
			log.Printf("[Importer] Warning: failed to create conversation, might already exist")
			continue
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
					text = "Mídia"
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
			time.Sleep(200 * time.Millisecond)
		}
		
		log.Printf("[Importer] Completed chat %s", phoneNumber)
	}

	log.Printf("[Importer] Finished history import for client %s", client.Name)
}
