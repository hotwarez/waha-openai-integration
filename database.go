package main

import (
    "log"
    "os"

    "github.com/glebarez/sqlite"
    "gorm.io/gorm"
)

type Client struct {
    ID                uint   `gorm:"primaryKey"`
    Name              string `gorm:"not null"`
    WebhookSlug       string `gorm:"uniqueIndex;not null"`
    WahaURL           string
    WahaToken         string
    OpenAIKey         string
    OpenAIAssistant   string
    HumanPauseHours   int    `gorm:"default:24"`
}

type GlobalSetting struct {
    ID        uint   `gorm:"primaryKey"`
    WahaURL   string
    WahaToken string
}

var DB *gorm.DB

func initDB() {
    var err error
    
    // Ensure data directory exists
    if _, err := os.Stat("data"); os.IsNotExist(err) {
        os.MkdirAll("data", 0755)
    }

    DB, err = gorm.Open(sqlite.Open("data/waha_saas.db"), &gorm.Config{})
    if err != nil {
        log.Fatalf("failed to connect database: %v", err)
    }

    // Auto Migrate the schema
    err = DB.AutoMigrate(&Client{}, &GlobalSetting{})
    if err != nil {
        log.Fatalf("failed to migrate database: %v", err)
    }
}
