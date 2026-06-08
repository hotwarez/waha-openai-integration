package main

import (
    "bytes"
    "crypto/rand"
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
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

const AppVersion = "v1.1.9"

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
    activeSessions sync.Map // Map[string]uint (token -> userID)
)

func hashPassword(password string) string {
    h := sha256.New()
    h.Write([]byte(password + "waha_saas_salt"))
    return hex.EncodeToString(h.Sum(nil))
}

func generateSessionToken() string {
    b := make([]byte, 32)
    rand.Read(b)
    return hex.EncodeToString(b)
}

func authMiddleware() gin.HandlerFunc {
    return func(c *gin.Context) {
        token, err := c.Cookie("session_token")
        if err != nil || token == "" {
            c.Redirect(http.StatusFound, "/login")
            c.Abort()
            return
        }
        userIDIf, ok := activeSessions.Load(token)
        if !ok {
            c.Redirect(http.StatusFound, "/login")
            c.Abort()
            return
        }
        var user User
        if err := DB.First(&user, userIDIf.(uint)).Error; err != nil {
            c.Redirect(http.StatusFound, "/login")
            c.Abort()
            return
        }
        c.Set("user", user)
        c.Next()
    }
}

func hasAccess(user User, clientID uint) bool {
    if user.Role == "admin" {
        return true
    }
    var allowed []uint
    if err := json.Unmarshal([]byte(user.AllowedClients), &allowed); err != nil {
        return false
    }
    for _, id := range allowed {
        if id == clientID {
            return true
        }
    }
    return false
}

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

    var userCount int64
    DB.Model(&User{}).Count(&userCount)
    if userCount == 0 {
        DB.Create(&User{
            Username: "admin",
            PasswordHash: hashPassword(cfg.AdminPassword),
            Role: "admin",
        })
        log.Println("Created default admin user.")
    }

    r := gin.Default()
    templ := template.Must(template.New("").ParseFS(templatesFS, "templates/*"))
    r.SetHTMLTemplate(templ)

    // Public Auth Routes
    r.GET("/login", func(c *gin.Context) {
        c.HTML(http.StatusOK, "login.html", nil)
    })
    
    r.GET("/", func(c *gin.Context) {
        c.Redirect(http.StatusFound, "/admin")
    })

    r.POST("/api/login", func(c *gin.Context) {
        var req struct {
            Username string `json:"username"`
            Password string `json:"password"`
        }
        if err := c.BindJSON(&req); err != nil {
            c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
            return
        }
        var u User
        if err := DB.Where("username = ?", req.Username).First(&u).Error; err != nil {
            c.JSON(http.StatusUnauthorized, gin.H{"error": "Usuário ou senha incorretos"})
            return
        }
        if u.PasswordHash != hashPassword(req.Password) {
            c.JSON(http.StatusUnauthorized, gin.H{"error": "Usuário ou senha incorretos"})
            return
        }
        
        token := generateSessionToken()
        activeSessions.Store(token, u.ID)
        c.SetCookie("session_token", token, 86400, "/", "", false, true)
        c.JSON(http.StatusOK, gin.H{"message": "Login successful"})
    })

    r.POST("/api/logout", func(c *gin.Context) {
        token, _ := c.Cookie("session_token")
        if token != "" {
            activeSessions.Delete(token)
        }
        c.SetCookie("session_token", "", -1, "/", "", false, true)
        c.JSON(http.StatusOK, gin.H{"message": "Logged out"})
    })

    // Admin Panel Routes (Protected)
    adminGroup := r.Group("/admin", authMiddleware())
    adminGroup.GET("", func(c *gin.Context) {
        c.HTML(http.StatusOK, "admin.html", gin.H{"Version": AppVersion})
    })

    // API Routes (Protected)
    api := r.Group("/api", authMiddleware())
    
    api.GET("/me", func(c *gin.Context) {
        userObj, _ := c.Get("user")
        c.JSON(http.StatusOK, userObj.(User))
    })
    api.GET("/clients", func(c *gin.Context) {
        userObj, _ := c.Get("user")
        user := userObj.(User)

        var allClients []Client
        DB.Find(&allClients)

        if user.Role == "admin" {
            c.JSON(http.StatusOK, allClients)
            return
        }

        var allowed []uint
        _ = json.Unmarshal([]byte(user.AllowedClients), &allowed)
        
        var filtered []Client
        for _, cl := range allClients {
            for _, id := range allowed {
                if cl.ID == id {
                    filtered = append(filtered, cl)
                    break
                }
            }
        }
        c.JSON(http.StatusOK, filtered)
    })
    api.POST("/clients", func(c *gin.Context) {
        userObj, _ := c.Get("user")
        user := userObj.(User)
        if user.Role != "admin" && !user.CanEdit {
            c.JSON(http.StatusForbidden, gin.H{"error": "Sem permissão"})
            return
        }

        var client Client
        if err := c.BindJSON(&client); err != nil {
            c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
            return
        }

        // Check if editing an existing client and has access
        if client.ID != 0 && !hasAccess(user, client.ID) {
            c.JSON(http.StatusForbidden, gin.H{"error": "Sem acesso a este cliente"})
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
        userObj, _ := c.Get("user")
        user := userObj.(User)
        if user.Role != "admin" && !user.CanEdit {
            c.JSON(http.StatusForbidden, gin.H{"error": "Sem permissão"})
            return
        }
        id, _ := strconv.Atoi(c.Param("id"))
        if !hasAccess(user, uint(id)) {
            c.JSON(http.StatusForbidden, gin.H{"error": "Sem permissão"})
            return
        }

        DB.Delete(&Client{}, id)
        c.Status(http.StatusOK)
    })



    api.POST("/clients/:id/restart-waha", func(c *gin.Context) {
        userObj, _ := c.Get("user")
        user := userObj.(User)
        id, _ := strconv.Atoi(c.Param("id"))
        if !hasAccess(user, uint(id)) || (user.Role != "admin" && !user.CanEdit) {
            c.JSON(http.StatusForbidden, gin.H{"error": "Sem permissão"})
            return
        }

        var cl Client
        if err := DB.First(&cl, id).Error; err != nil {
            c.JSON(http.StatusNotFound, gin.H{"error": "client not found"})
            return
        }

        var global GlobalSetting
        DB.FirstOrCreate(&global, GlobalSetting{ID: 1})
        wahaURL := strings.TrimRight(global.WahaURL, "/")
        wahaToken := global.WahaToken

        req := resty.New().R()
        if wahaToken != "" {
            req.SetHeader("X-Api-Key", wahaToken)
            req.SetHeader("Authorization", "Bearer "+wahaToken)
        }
        
        // 1. Stop session
        req.SetBody(map[string]interface{}{"name": cl.WahaSession})
        req.Post(wahaURL + "/api/sessions/stop")
        
        // 2. Start session
        resp, err := req.Post(wahaURL + "/api/sessions/start")
        if err != nil {
            c.JSON(http.StatusInternalServerError, gin.H{"error": "Erro ao reiniciar sessão WAHA"})
            return
        }
        
        if resp.IsError() {
            c.JSON(http.StatusInternalServerError, gin.H{"error": "Erro do WAHA ao iniciar: " + resp.String()})
            return
        }
        c.JSON(http.StatusOK, gin.H{"message": "Sessão WAHA reiniciada com sucesso"})
    })

    api.POST("/clients/:id/unpause", func(c *gin.Context) {
        userObj, _ := c.Get("user")
        user := userObj.(User)
        id, _ := strconv.Atoi(c.Param("id"))
        if !hasAccess(user, uint(id)) || (user.Role != "admin" && !user.CanPause) {
            c.JSON(http.StatusForbidden, gin.H{"error": "Sem permissão"})
            return
        }

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

    api.GET("/clients/:id/qr", func(c *gin.Context) {
        userObj, _ := c.Get("user")
        user := userObj.(User)
        id, _ := strconv.Atoi(c.Param("id"))
        if !hasAccess(user, uint(id)) || (user.Role != "admin" && !user.CanViewQR) {
            c.JSON(http.StatusForbidden, gin.H{"error": "Sem permissão"})
            return
        }

        var cl Client
        if err := DB.First(&cl, id).Error; err != nil {
            c.JSON(http.StatusNotFound, gin.H{"error": "client not found"})
            return
        }
        
        var global GlobalSetting
        DB.FirstOrCreate(&global, GlobalSetting{ID: 1})
        if global.WahaURL == "" {
            c.JSON(http.StatusBadRequest, gin.H{"error": "WAHA URL global não configurada"})
            return
        }

        sessionName := cl.WahaSession
        if sessionName == "" {
            c.JSON(http.StatusBadRequest, gin.H{"error": "Sessão WAHA não configurada para este cliente"})
            return
        }

        wahaURL := strings.TrimRight(global.WahaURL, "/")
        wahaToken := global.WahaToken

        // 1. Try to start the session in case it's stopped/doesn't exist
        startReq := resty.New().R().
            SetHeader("Content-Type", "application/json")
        if wahaToken != "" {
            startReq.SetHeader("X-Api-Key", wahaToken)
            startReq.SetHeader("Authorization", "Bearer "+wahaToken)
        }
        startBody := map[string]interface{}{"name": sessionName}
                
                var g GlobalSetting
                DB.First(&g, 1)
                if g.BackendURL != "" {
                    webhookURL := g.BackendURL
                    if !strings.HasSuffix(webhookURL, "/") {
                        webhookURL += "/"
                    }
                    webhookURL += "webhook/" + cl.WebhookSlug
                    
                    startBody["config"] = map[string]interface{}{
                        "webhooks": []map[string]interface{}{
                            {
                                "url": webhookURL,
                                "events": []string{"message", "message.any", "session.status"},
                            },
                        },
                    }
                }
                
                startReq.SetBody(startBody)
        startReq.Post(wahaURL + "/api/sessions/start") // Best effort, ignore errors

        // 2. Fetch QR Code
        qrReq := resty.New().R()
        if wahaToken != "" {
            qrReq.SetHeader("X-Api-Key", wahaToken)
            qrReq.SetHeader("Authorization", "Bearer "+wahaToken)
        }
        
        // In some WAHA versions the route is /api/{session}/auth/qr, in others it's /api/sessions/{session}/auth/qr
        // Let's try /api/{session}/auth/qr first
        resp, err := qrReq.Get(wahaURL + "/api/" + sessionName + "/auth/qr")
        if err != nil {
            c.JSON(http.StatusInternalServerError, gin.H{"error": "Erro ao conectar com WAHA"})
            return
        }

        // Fallback if 404
        if resp.StatusCode() == 404 {
            resp, err = qrReq.Get(wahaURL + "/api/sessions/" + sessionName + "/auth/qr")
            if err != nil {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "Erro ao conectar com WAHA"})
                return
            }
        }

        if resp.IsError() {
            bodyStr := resp.String()
            // Auto-restart if session is FAILED
            if strings.Contains(bodyStr, "FAILED") || strings.Contains(bodyStr, "restart") {
                // Stop the session
                stopReq := resty.New().R()
                if wahaToken != "" {
                    stopReq.SetHeader("X-Api-Key", wahaToken)
                    stopReq.SetHeader("Authorization", "Bearer "+wahaToken)
                }
                stopReq.SetBody(map[string]interface{}{"name": sessionName})
                stopReq.Post(wahaURL + "/api/sessions/stop")
                
                // Start again
                startReq.Post(wahaURL + "/api/sessions/start")
                
                // Try to get QR again
                resp, err = qrReq.Get(wahaURL + "/api/" + sessionName + "/auth/qr")
                if err == nil && resp.StatusCode() == 404 {
                    resp, err = qrReq.Get(wahaURL + "/api/sessions/" + sessionName + "/auth/qr")
                }
            }
            
            if err != nil || resp.IsError() {
                c.JSON(http.StatusInternalServerError, gin.H{"error": "WAHA retornou erro: " + resp.String()})
                return
            }
        }

        // Return WAHA's response directly to frontend (usually {"session": "...", "mimetype": "...", "data": "..."} or {"url": "..."})
        c.Data(resp.StatusCode(), resp.Header().Get("Content-Type"), resp.Body())
    })

    api.POST("/clients/:id/pause", func(c *gin.Context) {
        userObj, _ := c.Get("user")
        user := userObj.(User)
        id, _ := strconv.Atoi(c.Param("id"))
        if !hasAccess(user, uint(id)) || (user.Role != "admin" && !user.CanPause) {
            c.JSON(http.StatusForbidden, gin.H{"error": "Sem permissão"})
            return
        }

        var cl Client
        if err := DB.First(&cl, id).Error; err != nil {
            c.JSON(http.StatusNotFound, gin.H{"error": "client not found"})
            return
        }
        DB.Model(&cl).Update("paused", true)
        c.JSON(http.StatusOK, gin.H{"message": "Bot pausado globalmente para " + cl.Name})
    })

    api.GET("/settings", func(c *gin.Context) {
        userObj, _ := c.Get("user")
        if userObj.(User).Role != "admin" {
            c.JSON(http.StatusForbidden, gin.H{"error": "Sem permissão"})
            return
        }
        var global GlobalSetting
        DB.FirstOrCreate(&global, GlobalSetting{ID: 1})
        c.JSON(http.StatusOK, global)
    })
    api.POST("/settings", func(c *gin.Context) {
        userObj, _ := c.Get("user")
        if userObj.(User).Role != "admin" {
            c.JSON(http.StatusForbidden, gin.H{"error": "Sem permissão"})
            return
        }
        var global GlobalSetting
        if err := c.BindJSON(&global); err != nil {
            c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
            return
        }
        global.ID = 1
        DB.Save(&global)
        c.JSON(http.StatusOK, global)
    })

    // User Management API
    api.GET("/users", func(c *gin.Context) {
        userObj, _ := c.Get("user")
        if userObj.(User).Role != "admin" {
            c.JSON(http.StatusForbidden, gin.H{"error": "Sem permissão"})
            return
        }
        var users []User
        DB.Select("id, username, role, allowed_clients, can_edit, can_pause, can_view_qr").Find(&users)
        c.JSON(http.StatusOK, users)
    })
    
    api.POST("/users", func(c *gin.Context) {
        userObj, _ := c.Get("user")
        if userObj.(User).Role != "admin" {
            c.JSON(http.StatusForbidden, gin.H{"error": "Sem permissão"})
            return
        }
        var req struct {
            ID             uint   `json:"ID"`
            Username       string `json:"Username"`
            Password       string `json:"Password"` // plain text, optional if edit
            Role           string `json:"Role"`
            AllowedClients string `json:"AllowedClients"`
            CanEdit        bool   `json:"CanEdit"`
            CanPause       bool   `json:"CanPause"`
            CanViewQR      bool   `json:"CanViewQR"`
        }
        if err := c.BindJSON(&req); err != nil {
            c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
            return
        }
        
        var u User
        if req.ID != 0 {
            DB.First(&u, req.ID)
        }
        u.Username = req.Username
        u.Role = req.Role
        u.AllowedClients = req.AllowedClients
        u.CanEdit = req.CanEdit
        u.CanPause = req.CanPause
        u.CanViewQR = req.CanViewQR
        
        if req.Password != "" {
            u.PasswordHash = hashPassword(req.Password)
        }

        if err := DB.Save(&u).Error; err != nil {
            c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
            return
        }
        c.JSON(http.StatusOK, gin.H{"message": "User saved"})
    })
    
    api.DELETE("/users/:id", func(c *gin.Context) {
        userObj, _ := c.Get("user")
        if userObj.(User).Role != "admin" {
            c.JSON(http.StatusForbidden, gin.H{"error": "Sem permissão"})
            return
        }
        id := c.Param("id")
        if id == strconv.Itoa(int(userObj.(User).ID)) {
            c.JSON(http.StatusBadRequest, gin.H{"error": "Não pode deletar a si mesmo"})
            return
        }
        DB.Delete(&User{}, id)
        c.Status(http.StatusOK)
    })

    



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
            
            // Extract real JID from _data.Info.SenderAlt if available (for LID resolution)
            if _data, ok := innerPayload["_data"].(map[string]interface{}); ok {
                if info, ok := _data["Info"].(map[string]interface{}); ok {
                    senderAlt, _ := info["SenderAlt"].(string)
                    if senderAlt != "" && strings.Contains(senderAlt, "@s.whatsapp.net") {
                        chatID = strings.Replace(senderAlt, "@s.whatsapp.net", "@c.us", 1)
                    }
                }
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

        // Parse timestamp
        var ts int64
        if timestampFloat, ok := payload["timestamp"].(float64); ok {
            ts = int64(timestampFloat)
        }
        
        // Safety check: Ignore messages older than 5 minutes (300 seconds)
        // This prevents WAHA native history sync from triggering AI replies
        if ts > 0 && time.Now().Unix()-ts > 300 {
            log.Printf("[%s] Ignoring old message (history sync). Chat: %s, Ts: %d", slug, chatID, ts)
            c.Status(http.StatusOK)
            return
        }

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
