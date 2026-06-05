package main

import (
    "bytes"
    "encoding/json"
    "fmt"
    "io"
    "log"
    "net/http"
    "strings"
    "time"

    "github.com/gin-gonic/gin"
    "github.com/go-resty/resty/v2"
)

// sendLiveMessageToChatwoot pushes a new WAHA message to Chatwoot instantly
func sendLiveMessageToChatwoot(client Client, chatID, text string, fromMe bool, timestamp int64) {
    if client.ChatwootAccountID == 0 || client.ChatwootInboxID == 0 {
        return
    }

    var global GlobalSetting
    DB.FirstOrCreate(&global, GlobalSetting{ID: 1})
    cwURL := strings.TrimRight(global.ChatwootURL, "/")
    cwToken := global.ChatwootToken

    if cwURL == "" || cwToken == "" {
        return
    }

    // Prepare API URL and headers
    apiURL := fmt.Sprintf("%s/api/v1/accounts/%d", cwURL, client.ChatwootAccountID)
    cwClient := resty.New().
        SetHeader("api_access_token", cwToken).
        SetHeader("Content-Type", "application/json")

    // 1. Check or Create Contact
    phone := strings.Split(chatID, "@")[0]
    searchResp, err := cwClient.R().Get(apiURL + "/contacts/search?q=" + phone)
    if err != nil {
        log.Printf("Chatwoot: failed to search contact: %v", err)
        return
    }

    var searchRes map[string]interface{}
    json.Unmarshal(searchResp.Body(), &searchRes)

    var contactID float64
    payloadBytes, _ := json.Marshal(searchRes["payload"])
    var payloads []map[string]interface{}
    json.Unmarshal(payloadBytes, &payloads)

    if len(payloads) > 0 && payloads[0]["id"] != nil {
        contactID = payloads[0]["id"].(float64)
    } else {
        // Create contact
        createBody := map[string]interface{}{
            "name":         phone,
            "phone_number": "+" + phone,
            "identifier":   chatID,
        }
        createResp, err := cwClient.R().SetBody(createBody).Post(apiURL + "/contacts")
        if err != nil {
            log.Printf("Chatwoot: failed to create contact: %v", err)
            return
        }
        var createRes map[string]interface{}
        json.Unmarshal(createResp.Body(), &createRes)
        if payload, ok := createRes["payload"].(map[string]interface{}); ok && payload["contact"] != nil {
            contactBody := payload["contact"].(map[string]interface{})
            contactID = contactBody["id"].(float64)
        } else {
            log.Printf("Chatwoot: failed to get created contact ID: %s", createResp.String())
            return
        }
    }

    // 2. Check or Create Conversation
    convResp, err := cwClient.R().Get(fmt.Sprintf("%s/conversations?inbox_id=%d", apiURL, client.ChatwootInboxID))
    if err != nil {
        log.Printf("Chatwoot: failed to get conversations: %v", err)
        return
    }
    var convRes map[string]interface{}
    json.Unmarshal(convResp.Body(), &convRes)

    var conversationID float64
    if data, ok := convRes["data"].(map[string]interface{}); ok {
        if meta, ok := data["meta"].(map[string]interface{}); ok {
            if count, ok := meta["mine_count"].(float64); ok && count >= 0 {
                if payload, ok := data["payload"].([]interface{}); ok {
                    for _, p := range payload {
                        conv := p.(map[string]interface{})
                        metaInfo := conv["meta"].(map[string]interface{})
                        sender := metaInfo["sender"].(map[string]interface{})
                        if sender["id"].(float64) == contactID {
                            conversationID = conv["id"].(float64)
                            break
                        }
                    }
                }
            }
        }
    }

    if conversationID == 0 {
        // Create conversation
        createConvBody := map[string]interface{}{
            "source_id":  phone,
            "inbox_id":   client.ChatwootInboxID,
            "contact_id": contactID,
        }
        createConvResp, err := cwClient.R().SetBody(createConvBody).Post(apiURL + "/conversations")
        if err != nil {
            log.Printf("Chatwoot: failed to create conversation: %v", err)
            return
        }
        var createConvRes map[string]interface{}
        json.Unmarshal(createConvResp.Body(), &createConvRes)
        if createConvRes["id"] != nil {
            conversationID = createConvRes["id"].(float64)
        } else {
            log.Printf("Chatwoot: failed to get created conversation ID: %s", createConvResp.String())
            return
        }
    }

    // 3. Push Message
    msgType := "incoming"
    if fromMe {
        msgType = "outgoing"
    }

    msgBody := map[string]interface{}{
        "content":      text,
        "message_type": msgType,
        "private":      false,
    }

    if timestamp > 0 {
        msgBody["created_at"] = time.Unix(timestamp, 0).Format(time.RFC3339)
    }

    _, err = cwClient.R().SetBody(msgBody).Post(fmt.Sprintf("%s/conversations/%d/messages", apiURL, int(conversationID)))
    if err != nil {
        log.Printf("Chatwoot: failed to push message: %v", err)
    }
}

// setupChatwootWebhook registers the Chatwoot -> WAHA reverse webhook
func setupChatwootWebhook(r *gin.Engine) {
    r.POST("/chatwoot-webhook", func(c *gin.Context) {
        bodyBytes, _ := io.ReadAll(c.Request.Body)
        c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

        var payload map[string]interface{}
        if err := c.BindJSON(&payload); err != nil {
            c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
            return
        }

        // Only process outgoing messages created by human agents
        event, _ := payload["event"].(string)
        messageType, _ := payload["message_type"].(string) // "outgoing"
        isPrivate, _ := payload["private"].(bool)
        
        if event != "message_created" || messageType != "outgoing" || isPrivate {
            c.Status(http.StatusOK)
            return
        }

        // Check if message was sent by an automation/API instead of human
        sender, hasSender := payload["sender"].(map[string]interface{})
        if !hasSender {
            c.Status(http.StatusOK)
            return
        }
        
        // Chatwoot agents have "type": "user"
        senderType, _ := sender["type"].(string)
        if senderType != "user" {
            c.Status(http.StatusOK)
            return
        }

        // Get the Inbox ID and Phone
        inbox, hasInbox := payload["inbox"].(map[string]interface{})
        if !hasInbox {
            c.Status(http.StatusOK)
            return
        }
        inboxIDFloat, _ := inbox["id"].(float64)
        inboxID := int(inboxIDFloat)

        conversation, hasConv := payload["conversation"].(map[string]interface{})
        if !hasConv {
            c.Status(http.StatusOK)
            return
        }
        
        meta, hasMeta := conversation["meta"].(map[string]interface{})
        if !hasMeta {
            c.Status(http.StatusOK)
            return
        }
        contactSender, _ := meta["sender"].(map[string]interface{})
        phoneNumber, _ := contactSender["phone_number"].(string) // +5511...
        identifier, _ := contactSender["identifier"].(string)
        
        var chatID string
        if identifier != "" {
            chatID = identifier
        } else if phoneNumber != "" {
            phoneWithoutPlus := strings.TrimPrefix(phoneNumber, "+")
            chatID = phoneWithoutPlus + "@c.us"
        } else {
            c.Status(http.StatusOK)
            return
        }


        // Find which client owns this Inbox ID
        var client Client
        if err := DB.Where("chatwoot_inbox_id = ?", inboxID).First(&client).Error; err != nil {
            log.Printf("Chatwoot Webhook: no client found for inbox ID %d", inboxID)
            c.Status(http.StatusOK)
            return
        }

        content, _ := payload["content"].(string)
        if content == "" {
            c.Status(http.StatusOK)
            return
        }

        // SAFETY LOCK: Block any message containing [Histórico: or [History:
        // This prevents massive SPAM bans if a history sync triggers Chatwoot webhooks
        if strings.Contains(content, "[Histórico:") || strings.Contains(content, "[History:") {
            log.Printf("Chatwoot Webhook: Blocked imported history message from being sent to WhatsApp: %s", chatID)
            c.Status(http.StatusOK)
            return
        }

        // SAFETY LOCK 2: Block if message is old (from created_at attribute)
        if createdAtStr, ok := payload["created_at"].(string); ok {
            if createdAt, err := time.Parse(time.RFC3339, createdAtStr); err == nil {
                if time.Now().Sub(createdAt) > 5 * time.Minute {
                    log.Printf("Chatwoot Webhook: Blocked old message from being sent to WhatsApp: %s", chatID)
                    c.Status(http.StatusOK)
                    return
                }
            }
        }

        // Send to WAHA
        
        log.Printf("Chatwoot Webhook: sending '%s' to %s via session %s", content, chatID, client.WahaSession)
        sendMessage(client, client.WahaSession, chatID, content)

        // TRIGGER HUMAN HANDOFF PAUSE!
        pauseKey := client.WebhookSlug + ":" + chatID
        humanPauses.Store(pauseKey, time.Now())
        log.Printf("Chatwoot Webhook: Human Handoff triggered. Paused bot for %s", chatID)

        c.Status(http.StatusOK)
    })
}
