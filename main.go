package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	APIKey         = "YOUR_API_KEY"
	ReconnectDelay = 5 * time.Second
	MaxRetries     = 5
	PingInterval   = 45 * time.Second
)

type DotaMarketWatcher struct {
	conn         *websocket.Conn
	token        string
	tokenExpires time.Time
	retries      int
	lastPing     time.Time
	logger       *log.Logger
}

func createLogger() (*log.Logger, error) {
	os.MkdirAll("logs", 0755)
	logFileName := fmt.Sprintf("logs/market_watcher_%s.log", time.Now().Format("20060102_150405"))
	file, err := os.Create(logFileName)
	if err != nil {
		return nil, err
	}
	return log.New(file, "", log.LstdFlags), nil
}

func (d *DotaMarketWatcher) Initialize() error {
	d.retries = 0
	if err := d.UpdateToken(); err != nil {
		return err
	}
	return d.Connect()
}

func (d *DotaMarketWatcher) UpdateToken() error {
	url := fmt.Sprintf("https://market.csgo.com/api/v2/get-ws-token?key=%s", APIKey)
	resp, err := http.Post(url, "application/json", nil)
	if err != nil {
		d.logger.Printf("Token request error: %v", err)
		return err
	}
	defer resp.Body.Close()

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		d.logger.Printf("Response read error: %v", err)
		return err
	}

	var data struct {
		Success bool   `json:"success"`
		Token   string `json:"token"`
		Error   string `json:"error"`
	}
	if err = json.Unmarshal(body, &data); err != nil {
		d.logger.Printf("Token parse error: %v", err)
		return err
	}

	if data.Success {
		d.token = data.Token
		d.tokenExpires = time.Now().Add(9 * time.Minute)
		d.logger.Println("Token updated")
		return nil
	}

	d.logger.Printf("Token error: %s", data.Error)
	return fmt.Errorf("token error: %s", data.Error)
}

func (d *DotaMarketWatcher) Connect() error {
	if time.Now().After(d.tokenExpires) {
		if err := d.UpdateToken(); err != nil {
			return err
		}
	}

	d.logger.Println("Connecting to WebSocket...")
	conn, _, err := websocket.DefaultDialer.Dial(
		"wss://wsn.dota2.net/wsn/",
		http.Header{
			"Origin":     []string{"https://market.csgo.com"},
			"User-Agent": []string{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/91.0.4472.124 Safari/537.36"},
		},
	)
	if err != nil {
		d.logger.Printf("Connection error: %v", err)
		return err
	}

	d.conn = conn
	if d.token != "" {
		if err = d.conn.WriteMessage(websocket.TextMessage, []byte(d.token)); err != nil {
			d.logger.Printf("Token send error: %v", err)
			return err
		}
	}

	for _, channel := range []string{"newitems_go"} {
		if err = d.conn.WriteMessage(websocket.TextMessage, []byte(channel)); err != nil {
			d.logger.Printf("Subscribe error: %v", err)
			return err
		}
	}

	d.logger.Println("Connected successfully")
	return nil
}

func (d *DotaMarketWatcher) processMessage(message []byte) {
	var data map[string]interface{}
	if err := json.Unmarshal(message, &data); err != nil {
		d.logger.Printf("Non-JSON message: %s", message)
		return
	}

	if data["type"] == "newitems_go" {
		itemData := make(map[string]interface{})
		if err := json.Unmarshal([]byte(data["data"].(string)), &itemData); err != nil {
			d.logger.Printf("Data parse error: %v", err)
			return
		}

		var buffer bytes.Buffer
		buffer.WriteString(fmt.Sprintf("\n%s\n", strings.Repeat("=", 50)))
		buffer.WriteString(fmt.Sprintf("Item: %s\n", getValue(itemData, "i_market_name")))
		buffer.WriteString(fmt.Sprintf("Quality: %s\n", getValue(itemData, "i_quality", "--")))
		buffer.WriteString(fmt.Sprintf("Price: %s %s\n",
			getValue(itemData, "ui_price"),
			getValue(itemData, "ui_currency")))

		if floatVal := getValue(itemData, "ui_float"); floatVal != "" && floatVal != "<nil>" {
			buffer.WriteString(fmt.Sprintf("Float: %s\n", floatVal))
		}

		if stickers, ok := itemData["stickers"].([]interface{}); ok && len(stickers) > 0 {
			buffer.WriteString("Stickers:\n")
			for _, s := range stickers {
				stickerID := fmt.Sprintf("%.0f", s.(float64))
				buffer.WriteString(fmt.Sprintf("  - ID: %s\n", stickerID))
			}
		}

		if inspectURL := getValue(itemData, "inspect_url"); inspectURL != "" {
			buffer.WriteString(fmt.Sprintf("Inspect: %s\n",
				strings.ReplaceAll(inspectURL, `\/`, `/`)))
		}

		buffer.WriteString(fmt.Sprintf("%s\n", strings.Repeat("=", 50)))
		d.logger.Println(buffer.String())
	}
}

func getValue(data map[string]interface{}, keys ...string) string {
	key := keys[0]
	defaultValue := ""
	if len(keys) > 1 {
		defaultValue = keys[1]
	}

	val, ok := data[key]
	if !ok {
		return defaultValue
	}

	switch v := val.(type) {
	case string:
		return v
	case float64:
		return fmt.Sprintf("%.2f", v)
	default:
		return fmt.Sprintf("%v", v)
	}
}

func (d *DotaMarketWatcher) Listen() error {
	defer d.conn.Close()

	ticker := time.NewTicker(PingInterval)
	defer ticker.Stop()

	done := make(chan error)
	go func() {
		for {
			_, msg, err := d.conn.ReadMessage()
			if err != nil {
				done <- err
				return
			}
			d.processMessage(msg)
		}
	}()

	for {
		select {
		case err := <-done:
			return err
		case <-ticker.C:
			if err := d.conn.WriteMessage(websocket.TextMessage, []byte("ping")); err != nil {
				return err
			}
			d.lastPing = time.Now()
		}
	}
}

func main() {
	logger, err := createLogger()
	if err != nil {
		log.Fatal("Logger creation failed:", err)
	}

	watcher := &DotaMarketWatcher{logger: logger}

	for {
		if err := watcher.Initialize(); err != nil {
			if watcher.retries >= MaxRetries {
				logger.Fatal("Max retries reached")
			}
			watcher.retries++
			logger.Printf("Reconnecting %d/%d\n", watcher.retries, MaxRetries)
			time.Sleep(ReconnectDelay)
			continue
		}

		if err := watcher.Listen(); err != nil {
			logger.Printf("Listen error: %v", err)
			watcher.conn.Close()
			if watcher.retries >= MaxRetries {
				logger.Fatal("Max retries reached")
			}
			watcher.retries++
			time.Sleep(ReconnectDelay)
		}
	}
}
