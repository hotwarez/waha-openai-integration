package main

import (
    "bytes"
    "embed"
    "fmt"
    "html/template"
    "io"
    "log"
    "net/http"
    "os"
    "strconv"
    "strings"
    "sync"
    "time"
    "github.com/gin-gonic/gin"
    "github.com/joho/godotenv"
    "github.com/go-resty/resty/v2"
    "github.com/tidwall/gjson"
)

type Config struct {
    UseResponsesAPI bool
    AdminPassword   string
}

//go:embed templates/*
var templatesFS embed.FS

var (
    client        = resty.New()
    sentBotMsgs   sync.Map // Map[msgID]bool
    humanPauses   sync.Map // Map[chatID]time.Time
)

func loadConfig() Config {
    _ = godotenv.Load()
    useResp, _ := strconv.ParseBool(os.Getenv("USE_RESPONSES_API"))
    pwd := os.Getenv("ADMIN_PASSWORD")
    if pwd == "" {
        pwd = "admin" // default fallback
    }
    return Config{
        UseResponsesAPI: useResp,
        AdminPassword:   pwd,
    }
}


func main() {
    initDB()
    cfg := loadConfig()

    r := gin.Default()
    templ := template.Must(template.New("").ParseFS(templatesFS, "templates/*"))
    r.SetHTMLTemplate(templ)

    // Admin Panel Routes (Protected)
    adminGroup := r.Group("/admin", gin.BasicAuth(gin.Accounts{
        "admin": cfg.AdminPassword,
    }))
    adminGroup.GET("", func(c *gin.Context) {
        c.HTML(http.StatusOK, "admin.html", nil)
    })

    // API Routes (Protected)
    api := r.Group("/api", gin.BasicAuth(gin.Accounts{
        "admin": cfg.AdminPassword,
    }))
    api.GET("/clients", func(c *gin.Context) {
        var clients []Client
        DB.Find(&clients)
        c.JSON(http.StatusOK, clients)
    })
    api.POST("/clients", func(c *gin.Context) {
        var client Client
        if err := c.BindJSON(&client); err != nil {
            c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
            return
        }
        client.WebhookSlug = strings.ToLower(strings.ReplaceAll(client.WebhookSlug, " ", "-"))
        if err := DB.Save(&client).Error; err != nil {
            c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save: " + err.Error()})
            return
        }
        c.JSON(http.StatusOK, client)
    })
    api.DELETE("/clients/:id", func(c *gin.Context) {
        id := c.Param("id")
        DB.Delete(&Client{}, id)
        c.Status(http.StatusOK)
    })

    api.POST("/clients/:id/unpause", func(c *gin.Context) {
        id := c.Param("id")
        var cl Client
        if err := DB.First(&cl, id).Error; err != nil {
            c.JSON(http.StatusNotFound, gin.H{"error": "client not found"})
            return
        }
        // Remove all human pauses for this client slug
        prefix := cl.WebhookSlug + ":"
        humanPauses.Range(func(key, _ interface{}) bool {
            if strings.HasPrefix(key.(string), prefix) {
                humanPauses.Delete(key)
                log.Printf("[%s] Pause manually cleared for key: %s", cl.WebhookSlug, key)
            }
            return true
        })
        DB.Model(&cl).Update("paused", false)
        c.JSON(http.StatusOK, gin.H{"message": "Bot despausado para " + cl.Name})
    })

    api.POST("/clients/:id/pause", func(c *gin.Context) {
        id := c.Param("id")
        var cl Client
        if err := DB.First(&cl, id).Error; err != nil {
            c.JSON(http.StatusNotFound, gin.H{"error": "client not found"})
            return
        }
        DB.Model(&cl).Update("paused", true)
        c.JSON(http.StatusOK, gin.H{"message": "Bot pausado globalmente para " + cl.Name})
    })

    api.GET("/settings", func(c *gin.Context) {
        var setting GlobalSetting
        DB.FirstOrCreate(&setting, GlobalSetting{ID: 1})
        c.JSON(http.StatusOK, setting)
    })

    api.POST("/settings", func(c *gin.Context) {
        var setting GlobalSetting
        if err := c.BindJSON(&setting); err != nil {
            c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
            return
        }
        setting.ID = 1 // Always force ID 1 for global settings
        if err := DB.Save(&setting).Error; err != nil {
            c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save settings: " + err.Error()})
            return
        }
        c.JSON(http.StatusOK, setting)
    })

    // Dynamic Webhook
    r.POST("/webhook/:slug", func(c *gin.Context) {
        slug := c.Param("slug")
        var client Client
        if err := DB.Where("webhook_slug = ?", slug).First(&client).Error; err != nil {
            log.Printf("Webhook error: Client not found for slug %s", slug)
            c.Status(http.StatusNotFound)
            return
        }

        bodyBytes, _ := io.ReadAll(c.Request.Body)
        log.Printf("[%s] Raw webhook: %s", slug, string(bodyBytes))
        c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

        var payload map[string]interface{}
        if err := c.BindJSON(&payload); err != nil {
            log.Printf("BindJSON error: %v", err)
            c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
            return
        }

        // Only process actual chat messages — ignore session.status and other WAHA system events
        eventType, _ := payload["event"].(string)
        if eventType != "" && eventType != "message.any" {
            log.Printf("[%s] Ignoring non-message event: %s", slug, eventType)
            c.Status(http.StatusOK)
            return
        }

        // Check global pause (set from admin panel)
        if client.Paused {
            log.Printf("[%s] Bot is globally paused. Ignoring message.", slug)
            c.Status(http.StatusOK)
            return
        }
        // Extract relevant fields (adjust according to WAHA schema)
        var chatID, text, session, msgID string
        var fromMe, hasMedia bool
        if innerPayload, ok := payload["payload"].(map[string]interface{}); ok {
            chatID, _ = innerPayload["from"].(string)
            if innerPayload["to"] != nil && innerPayload["to"] != "" && innerPayload["fromMe"] == true {
                chatID, _ = innerPayload["to"].(string)
            }
            text, _ = innerPayload["body"].(string)
            fromMe, _ = innerPayload["fromMe"].(bool)
            msgID, _ = innerPayload["id"].(string)
            hasMedia, _ = innerPayload["hasMedia"].(bool)
        } else {
            // Fallback for simple testing
            chatID, _ = payload["chatId"].(string)
            text, _ = payload["text"].(string)
        }
        session, _ = payload["session"].(string)
        if session == "" {
            session = "default"
        }
        log.Printf("[%s] Parsed session: '%s', chatID: '%s', text: '%s', fromMe: %v", slug, session, chatID, text, fromMe)
        
        pauseKey := slug + ":" + chatID

        // Handle fromMe (Human Handoff detection)
        if fromMe {
            if _, isBot := sentBotMsgs.Load(msgID); !isBot {
                // Sent by a human agent!
                log.Printf("[%s] Human intervention detected for chatID: %s", slug, chatID)
                humanPauses.Store(pauseKey, time.Now())
            } else {
                sentBotMsgs.Delete(msgID)
            }
            c.Status(http.StatusOK)
            return
        }

        // Check if user is currently paused
        if pauseTimeIf, ok := humanPauses.Load(pauseKey); ok {
            pauseTime := pauseTimeIf.(time.Time)
            if time.Since(pauseTime) < time.Duration(client.HumanPauseHours)*time.Hour {
                log.Printf("[%s] Ignoring message from %s (human pause active)", slug, chatID)
                c.Status(http.StatusOK)
                return
            } else {
                humanPauses.Delete(pauseKey)
            }
        }

        if chatID == "" {
            log.Printf("[%s] Error: missing chatID", slug)
            c.JSON(http.StatusBadRequest, gin.H{"error": "missing chatId"})
            return
        }

        // Handle audio messages — reply with custom message if configured
        if hasMedia && text == "" {
            audioMsg := client.AudioReplyMessage
            if audioMsg == "" {
                audioMsg = "Olá! Não consigo processar áudios. Por favor, envie sua mensagem em texto. 😊"
            }
            log.Printf("[%s] Audio message received from %s — sending audio reply", slug, chatID)
            sendMessage(client, session, chatID, audioMsg)
            c.Status(http.StatusOK)
            return
        }

        if text == "" {
            log.Printf("[%s] Empty text (non-audio media?), ignoring.", slug)
            c.Status(http.StatusOK)
            return
        }

        // Ignore Group Messages
        if strings.HasSuffix(chatID, "@g.us") || strings.Contains(chatID, "-") {
            log.Printf("[%s] Ignoring message from group chat: %s", slug, chatID)
            c.Status(http.StatusOK)
            return
        }

        // Process with OpenAI
        log.Printf("[%s] Calling OpenAI for chatID: %s", slug, chatID)
        reply, err := handleMessage(client, chatID, text)
        if err != nil {
            log.Printf("[%s] OpenAI error: %v", slug, err)
            // Send generic error back to user
            sendMessage(client, session, chatID, "Desculpe, ocorreu um erro ao processar sua mensagem.")
            c.Status(http.StatusOK)
            return
        }
        log.Printf("[%s] OpenAI reply generated: %s", slug, reply)
        // Send reply via WAHA
        sendMessage(client, session, chatID, reply)
        log.Printf("[%s] Reply sent to WAHA.", slug)
        c.Status(http.StatusOK)
    })
    // Health endpoint
    r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
    port := os.Getenv("PORT")
    if port == "" { port = "8080" }
    log.Printf("Server listening on :%s", port)
    r.Run(":" + port)
}

func handleMessage(client Client, chatID, userMsg string) (string, error) {
    // Simplified: using OpenAI Assistants API (run creation + polling)
    // 1. Ensure thread exists for this chatID
    threadID, err := getOrCreateThread(client, chatID)
    if err != nil { return "", err }
    // 2. Add user message to thread
    resp, err := resty.New().R().
        SetHeader("Authorization", "Bearer "+client.OpenAIKey).
        SetHeader("OpenAI-Beta", "assistants=v2").
        SetHeader("Content-Type", "application/json").
        SetBody(map[string]interface{}{"role": "user", "content": userMsg}).
        Post("https://api.openai.com/v1/threads/" + threadID + "/messages")
    if err != nil { return "", err }
    if resp.IsError() { return "", fmt.Errorf("OpenAI message error: %s", resp.String()) }
    
    // 3. Create a run
    runResp, err := resty.New().R().
        SetHeader("Authorization", "Bearer "+client.OpenAIKey).
        SetHeader("OpenAI-Beta", "assistants=v2").
        SetHeader("Content-Type", "application/json").
        SetBody(map[string]interface{}{"assistant_id": client.OpenAIAssistant}).
        Post("https://api.openai.com/v1/threads/" + threadID + "/runs")
    if err != nil { return "", err }
    if runResp.IsError() { return "", fmt.Errorf("OpenAI run error: %s", runResp.String()) }
    runID := gjson.GetBytes(runResp.Body(), "id").String()
    
    // 4. Poll for completion
    for {
        statusResp, err := resty.New().R().
            SetHeader("Authorization", "Bearer "+client.OpenAIKey).
            SetHeader("OpenAI-Beta", "assistants=v2").
            Get("https://api.openai.com/v1/threads/" + threadID + "/runs/" + runID)
        if err != nil { return "", err }
        if statusResp.IsError() { return "", fmt.Errorf("OpenAI status error: %s", statusResp.String()) }
        status := gjson.GetBytes(statusResp.Body(), "status").String()
        if status == "completed" { break }
        if status == "failed" { return "", fmt.Errorf("run failed: %s", statusResp.String()) }
        // simple back‑off
        time.Sleep(1 * time.Second)
    }
    
    // 5. Retrieve assistant messages
    msgsResp, err := resty.New().R().
        SetHeader("Authorization", "Bearer "+client.OpenAIKey).
        SetHeader("OpenAI-Beta", "assistants=v2").
        Get("https://api.openai.com/v1/threads/" + threadID + "/messages")
    if err != nil { return "", err }
    if msgsResp.IsError() { return "", fmt.Errorf("OpenAI messages error: %s", msgsResp.String()) }
    // Find the latest assistant message (index 0 is the newest in OpenAI API)
    msgs := gjson.GetBytes(msgsResp.Body(), "data").Array()
    for i := 0; i < len(msgs); i++ {
        msg := msgs[i]
        if msg.Get("role").String() == "assistant" {
            return msg.Get("content.0.text.value").String(), nil
        }
    }
    return "", fmt.Errorf("no assistant reply found")
}

func getOrCreateThread(client Client, chatID string) (string, error) {
    // Simple file‑based mapping
    // We isolate thread IDs per client
    path := "threads/" + client.WebhookSlug + "_" + chatID + ".txt"
    if data, err := os.ReadFile(path); err == nil {
        threadID := strings.TrimSpace(string(data))
        // Validate the stored thread ID — corrupted/empty files cause cryptic OpenAI errors
        if strings.HasPrefix(threadID, "thread_") {
            return threadID, nil
        }
        // File exists but is invalid, delete it and create a new thread
        log.Printf("Deleting corrupted thread file: %s (content: '%s')", path, threadID)
        os.Remove(path)
    }
    // Create new thread via OpenAI
    resp, err := resty.New().R().
        SetHeader("Authorization", "Bearer "+client.OpenAIKey).
        SetHeader("OpenAI-Beta", "assistants=v2").
        SetHeader("Content-Type", "application/json").
        SetBody(map[string]interface{}{}).
        Post("https://api.openai.com/v1/threads")
    if err != nil { return "", err }
    if resp.IsError() { return "", fmt.Errorf("failed to create thread: %s", resp.String()) }
    threadID := gjson.GetBytes(resp.Body(), "id").String()
    if !strings.HasPrefix(threadID, "thread_") {
        return "", fmt.Errorf("unexpected thread ID from OpenAI: '%s'", threadID)
    }
    os.MkdirAll("threads", 0755)
    os.WriteFile(path, []byte(threadID), 0644)
    return threadID, nil
}

func sendMessage(client Client, session, chatID, text string) {
    payload := map[string]string{
        "chatId":  chatID,
        "text":    text,
        "session": session,
    }
    
    // Determine WAHA config (fallback to GlobalSetting if client has none)
    wahaURL := client.WahaURL
    wahaToken := client.WahaToken
    if wahaURL == "" {
        var global GlobalSetting
        DB.FirstOrCreate(&global, GlobalSetting{ID: 1})
        wahaURL = global.WahaURL
        if client.WahaToken == "" {
            wahaToken = global.WahaToken
        }
    }

    if wahaURL == "" {
        log.Printf("WAHA Send Error: WAHA URL is not configured locally or globally!")
        return
    }

    r := resty.New().R().
        SetHeader("Content-Type", "application/json")
    if wahaToken != "" {
        r.SetHeader("Authorization", "Bearer "+wahaToken)
        r.SetHeader("X-Api-Key", wahaToken)
    }
    resp, err := r.SetBody(payload).
        Post(wahaURL + "/api/sendText")
    if err != nil {
        log.Printf("WAHA Send Error: %v", err)
    } else {
        log.Printf("WAHA Send Response: %s", resp.String())
        // Extract sent message ID to track it
        sentID := gjson.GetBytes(resp.Body(), "id").String()
        if sentID != "" {
            sentBotMsgs.Store(sentID, true)
            // auto cleanup after 5 minutes just in case webhook drops
            go func(id string) {
                time.Sleep(5 * time.Minute)
                sentBotMsgs.Delete(id)
            }(sentID)
        }
    }
}
