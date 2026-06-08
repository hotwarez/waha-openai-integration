package main

import (
	"bytes"
	"fmt"
	"log"
	"net/url"
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

		// Skip chats with no recent messages
		chatTs := chat.Get("timestamp").Int()
		if chatTs > 0 && chatTs < cutoffTime {
			continue
		}

		contactName := chat.Get("name").String()
		if contactName == "" {
			contactName = strings.Split(chatId, "@")[0]
		}
		phoneNumber := "+" + strings.Split(chatId, "@")[0]

		log.Printf("[Importer] Processing chat %s", phoneNumber)

		// 2. Get Messages for this chat (with downloadMedia=true)
		msgResp, err := req.Get(fmt.Sprintf("%s/api/%s/chats/%s/messages?limit=150&downloadMedia=true", wahaURL, sessionName, chatId))
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
			createResp, _ := cwClient.SetBody(map[string]interface{}{
				"name":         contactName,
				"phone_number": phoneNumber,
			}).Post(fmt.Sprintf("%s/api/v1/accounts/%d/contacts", cwURL, accountID))

			if createResp != nil && !createResp.IsError() {
				contactID = gjson.GetBytes(createResp.Body(), "payload.contact.id").Int()
				if contactID == 0 {
					contactID = gjson.GetBytes(createResp.Body(), "id").Int()
				}
			}
		}

		if contactID == 0 {
			log.Printf("[Importer] Failed to find/create contact %s", phoneNumber)
			continue
		}

		// 4. Find or Create Conversation
		convID := int64(0)

		// Try to find existing conversation first
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

		// Create if not found
		if convID == 0 {
			convResp, _ := cwClient.SetBody(map[string]interface{}{
				"inbox_id":   inboxID,
				"contact_id": contactID,
				"status":     "resolved",
			}).Post(fmt.Sprintf("%s/api/v1/accounts/%d/conversations", cwURL, accountID))

			if convResp != nil && !convResp.IsError() {
				convID = gjson.GetBytes(convResp.Body(), "id").Int()
			}
		}

		if convID == 0 {
			log.Printf("[Importer] Could not find or create conversation for %s", phoneNumber)
			continue
		}

		// 5. Send Messages in chronological order
		for i := len(messagesToImport) - 1; i >= 0; i-- {
			msg := messagesToImport[i]
			text := msg.Get("body").String()
			fromMe := msg.Get("fromMe").Bool()
			hasMedia := msg.Get("hasMedia").Bool()
			timestamp := msg.Get("timestamp").Int()

			if text == "" && !hasMedia {
				continue
			}

			// Build prefix with direction indicator
			direction := "Cliente"
			if fromMe {
				direction = "Atendente"
			}
			historyPrefix := fmt.Sprintf("[Historico %s: %s] - ", direction, time.Unix(timestamp, 0).Format("02/01/2006 15:04"))
			if text != "" {
				text = historyPrefix + text
			}

			// IMPORTANT: Always use message_type=0 (incoming) for imported history.
			// Using message_type=1 (outgoing) causes Chatwoot to try to SEND the
			// message via WhatsApp, resulting in "Failed to send" errors.
			// We differentiate direction via the text prefix instead.
			msgType := 0

			if !hasMedia {
				cwClient.SetBody(map[string]interface{}{
					"content":      text,
					"message_type": msgType,
					"private":      false,
				}).Post(fmt.Sprintf("%s/api/v1/accounts/%d/conversations/%d/messages", cwURL, accountID, convID))
			} else {
				// Has media - WAHA returns media.url when downloadMedia=true is used
				msgIdStr := msg.Get("id").String()
				mediaURL := msg.Get("media.url").String()
				mimetype := msg.Get("media.mimetype").String()
				mediaFilename := msg.Get("media.filename").String()

				// DEBUG: log the raw message JSON to diagnose media fields
				log.Printf("[Importer] DEBUG MEDIA msg id=%s mediaURL=%q mimetype=%q filename=%q raw=%s",
					msgIdStr, mediaURL, mimetype, mediaFilename, msg.Raw)

				// Strategy 1: use media.url directly from message (preferred when downloadMedia=true)
				// Strategy 2: fallback to /messages/{id}/download endpoint
				var mediaBytes []byte
				var contentType string


				if mediaURL != "" {
					// The URL from WAHA may be relative (e.g. /api/files/...) - make it absolute
					if strings.HasPrefix(mediaURL, "/") {
						mediaURL = wahaURL + mediaURL
					}
					log.Printf("[Importer] Downloading media via media.url: %s", mediaURL)
					mediaResp, dlErr := req.Get(mediaURL)
					if dlErr == nil && !mediaResp.IsError() {
						mediaBytes = mediaResp.Body()
						contentType = mediaResp.Header().Get("Content-Type")
					} else {
						log.Printf("[Importer] media.url download failed (status %d), trying /download endpoint", mediaResp.StatusCode())
					}
				}

				// Fallback: /messages/{id}/download
				if len(mediaBytes) == 0 {
					encodedMsgId := url.PathEscape(msgIdStr)
					mediaResp, dlErr := req.Get(wahaURL + "/api/" + sessionName + "/messages/" + encodedMsgId + "/download")
					if dlErr == nil && !mediaResp.IsError() {
						mediaBytes = mediaResp.Body()
						contentType = mediaResp.Header().Get("Content-Type")
					} else {
						log.Printf("[Importer] /download endpoint also failed (status %d) for msg %s", mediaResp.StatusCode(), msgIdStr)
					}
				}

				if len(mediaBytes) == 0 {
					// All download strategies failed - save as text placeholder
					fallbackText := historyPrefix + "[Midia]"
					if text != "" {
						fallbackText = text
					}
					cwClient.SetBody(map[string]interface{}{
						"content":      fallbackText,
						"message_type": msgType,
						"private":      false,
					}).Post(fmt.Sprintf("%s/api/v1/accounts/%d/conversations/%d/messages", cwURL, accountID, convID))
					time.Sleep(500 * time.Millisecond)
					continue
				}

				// Use mimetype from WAHA if available
				if mimetype != "" && contentType == "" {
					contentType = mimetype
				}

				// Determine content type and extension
				if contentType == "" {
					contentType = "application/octet-stream"
				}
				ext := ".bin"
				if strings.Contains(contentType, "image/jpeg") {
					ext = ".jpg"
				} else if strings.Contains(contentType, "image/png") {
					ext = ".png"
				} else if strings.Contains(contentType, "audio/ogg") {
					ext = ".ogg"
				} else if strings.Contains(contentType, "audio/mpeg") {
					ext = ".mp3"
				} else if strings.Contains(contentType, "video/mp4") {
					ext = ".mp4"
				} else if strings.Contains(contentType, "application/pdf") {
					ext = ".pdf"
				}

				// Use original filename from WAHA if available, otherwise generate one
				filename := mediaFilename
				if filename == "" {
					filename = "media_" + strconv.FormatInt(timestamp, 10) + ext
				}

				if text == "" {
					text = historyPrefix + "Midia"
				}

				_, uploadErr := resty.New().R().
					SetHeader("api_access_token", cwToken).
					SetMultipartFormData(map[string]string{
						"content":      text,
						"message_type": strconv.Itoa(msgType),
						"private":      "false",
					}).
					SetMultipartField("attachments[]", filename, contentType, bytes.NewReader(mediaBytes)).
					Post(fmt.Sprintf("%s/api/v1/accounts/%d/conversations/%d/messages", cwURL, accountID, convID))

				if uploadErr != nil {
					log.Printf("[Importer] Failed to upload media for %s: %v", msgIdStr, uploadErr)
				} else {
					log.Printf("[Importer] Media uploaded OK: %s", filename)
				}
			}

			// Small delay to prevent rate limiting
			time.Sleep(500 * time.Millisecond)
		}

		log.Printf("[Importer] Completed chat %s", phoneNumber)
	}

	log.Printf("[Importer] Finished history import for client %s", client.Name)
}
