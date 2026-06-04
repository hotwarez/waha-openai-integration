package main

import (
    "bytes"
    "fmt"
    "io"
    "log"
    "net/http"
    "os"
    "strconv"
    "time"
    "github.com/gin-gonic/gin"
    "github.com/joho/godotenv"
    "github.com/go-resty/resty/v2"
    "github.com/tidwall/gjson"
)

type Config struct {
    WahaURL       string
    WahaToken     string
    OpenAIKey     string
    UseResponses  bool
}

func loadConfig() Config {
    _ = godotenv.Load()
    useResp, _ := strconv.ParseBool(os.Getenv("USE_RESPONSES_API"))
    return Config{
        WahaURL:      os.Getenv("WAHA_URL"),
        WahaToken:    os.Getenv("WAHA_TOKEN"),
        OpenAIKey:    os.Getenv("OPENAI_API_KEY"),
        UseResponses: useResp,
    }
}

var client = resty.New()

func main() {
    cfg := loadConfig()
    if cfg.WahaURL == "" || cfg.OpenAIKey == "" {
        log.Fatal("WAHA_URL and OPENAI_API_KEY must be set")
    }
    r := gin.Default()
    r.POST("/webhook", func(c *gin.Context) {
        bodyBytes, _ := io.ReadAll(c.Request.Body)
        log.Printf("Raw webhook: %s", string(bodyBytes))
        c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

        var payload map[string]interface{}
        if err := c.BindJSON(&payload); err != nil {
            log.Printf("BindJSON error: %v", err)
            c.JSON(http.StatusBadRequest, gin.H{"error": "invalid payload"})
            return
        }
        // Extract relevant fields (adjust according to WAHA schema)
        var chatID, text string
        if innerPayload, ok := payload["payload"].(map[string]interface{}); ok {
            chatID, _ = innerPayload["from"].(string)
            text, _ = innerPayload["body"].(string)
        } else {
            // Fallback for simple testing
            chatID, _ = payload["chatId"].(string)
            text, _ = payload["text"].(string)
        }
        log.Printf("Parsed chatID: '%s', text: '%s'", chatID, text)
        if chatID == "" || text == "" {
            log.Printf("Error: missing chatID or text")
            c.JSON(http.StatusBadRequest, gin.H{"error": "missing chatId or text"})
            return
        }
        // Process with OpenAI
        log.Printf("Calling OpenAI for chatID: %s", chatID)
        reply, err := handleMessage(cfg, chatID, text)
        if err != nil {
            log.Printf("OpenAI error: %v", err)
            // Send generic error back to user
            sendMessage(cfg, chatID, "Desculpe, ocorreu um erro ao processar sua mensagem.")
            c.Status(http.StatusOK)
            return
        }
        log.Printf("OpenAI reply generated: %s", reply)
        // Send reply via WAHA
        sendMessage(cfg, chatID, reply)
        log.Printf("Reply sent to WAHA.")
        c.Status(http.StatusOK)
    })
    // Health endpoint
    r.GET("/health", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
    port := os.Getenv("PORT")
    if port == "" { port = "8080" }
    log.Printf("Server listening on :%s", port)
    r.Run(":" + port)
}

func handleMessage(cfg Config, chatID, userMsg string) (string, error) {
    // Simplified: using OpenAI Assistants API (run creation + polling)
    // 1. Ensure thread exists for this chatID
    threadID, err := getOrCreateThread(cfg, chatID)
    if err != nil { return "", err }
    // 2. Add user message to thread
    resp, err := client.R().
        SetHeader("Authorization", "Bearer "+cfg.OpenAIKey).
        SetHeader("Content-Type", "application/json").
        SetBody(map[string]interface{}{"role": "user", "content": userMsg}).
        Post("https://api.openai.com/v1/threads/" + threadID + "/messages")
    if err != nil { return "", err }
    if resp.IsError() { return "", fmt.Errorf("OpenAI message error: %s", resp.String()) }
    
    // 3. Create a run
    runResp, err := client.R().
        SetHeader("Authorization", "Bearer "+cfg.OpenAIKey).
        SetHeader("Content-Type", "application/json").
        SetBody(map[string]interface{}{"assistant_id": os.Getenv("OPENAI_ASSISTANT_ID")}).
        Post("https://api.openai.com/v1/threads/" + threadID + "/runs")
    if err != nil { return "", err }
    if runResp.IsError() { return "", fmt.Errorf("OpenAI run error: %s", runResp.String()) }
    runID := gjson.GetBytes(runResp.Body(), "id").String()
    
    // 4. Poll for completion
    for {
        statusResp, err := client.R().
            SetHeader("Authorization", "Bearer "+cfg.OpenAIKey).
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
    msgsResp, err := client.R().
        SetHeader("Authorization", "Bearer "+cfg.OpenAIKey).
        Get("https://api.openai.com/v1/threads/" + threadID + "/messages")
    if err != nil { return "", err }
    if msgsResp.IsError() { return "", fmt.Errorf("OpenAI messages error: %s", msgsResp.String()) }
    // Find the latest assistant message
    msgs := gjson.GetBytes(msgsResp.Body(), "data").Array()
    for i := len(msgs) - 1; i >= 0; i-- {
        role := msgs[i].Get("role").String()
        if role == "assistant" {
            return msgs[i].Get("content.0.text.value").String(), nil
        }
    }
    return "", fmt.Errorf("no assistant reply found")
}

func getOrCreateThread(cfg Config, chatID string) (string, error) {
    // Simple file‑based mapping (could be replaced by DB)
    // Here we store mapping in ./threads/<chatID>.txt containing threadID
    path := "threads/" + chatID + ".txt"
    if data, err := os.ReadFile(path); err == nil {
        return string(data), nil
    }
    // Create new thread via OpenAI
    resp, err := client.R().
        SetHeader("Authorization", "Bearer "+cfg.OpenAIKey).
        SetHeader("Content-Type", "application/json").
        SetBody(map[string]interface{}{}).
        Post("https://api.openai.com/v1/threads")
    if err != nil { return "", err }
    threadID := gjson.GetBytes(resp.Body(), "id").String()
    os.MkdirAll("threads", 0755)
    os.WriteFile(path, []byte(threadID), 0644)
    return threadID, nil
}

func sendMessage(cfg Config, chatID, text string) {
    payload := map[string]string{
        "chatId": chatID,
        "text":   text,
        "session": "default",
    }
    r := client.R().
        SetHeader("Content-Type", "application/json")
    if cfg.WahaToken != "" {
        r.SetHeader("Authorization", "Bearer "+cfg.WahaToken)
    }
    resp, err := r.SetBody(payload).
        Post(cfg.WahaURL + "/api/sendText")
    if err != nil {
        log.Printf("WAHA Send Error: %v", err)
    } else {
        log.Printf("WAHA Send Response: %s", resp.String())
    }
}
