package main

import (
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

type Client struct {
	conn     *websocket.Conn
	nickname string
	room     string
	avatar   string
}

type Message struct {
	Room     string `json:"room"`
	Nickname string `json:"nickname"`
	Avatar   string `json:"avatar"`
	Content  string `json:"content"`
	Type     string `json:"type"` // chat, vote, leave, switch
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

var (
	rooms        = make(map[string]map[*Client]bool)
	history      = make(map[string][]Message)
	votes        = make(map[string]map[string]int)
	broadcast    = make(chan Message)
	roomsMutex   sync.Mutex
	historyMutex sync.Mutex
	votesMutex   sync.Mutex
)

func main() {
	fs := http.FileServer(http.Dir("./static"))
	http.Handle("/", fs)
	http.HandleFunc("/ws", handleConnections)

	go handleMessages()

	fmt.Println("Server started on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleConnections(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	defer ws.Close()

	var initMsg Message
	err = ws.ReadJSON(&initMsg)
	if err != nil {
		log.Println("Init error:", err)
		return
	}

	client := &Client{
		conn:     ws,
		nickname: initMsg.Nickname,
		room:     initMsg.Room,
		avatar:   initMsg.Avatar,
	}

	roomsMutex.Lock()
	if rooms[client.room] == nil {
		rooms[client.room] = make(map[*Client]bool)
	}
	rooms[client.room][client] = true
	roomsMutex.Unlock()

	historyMutex.Lock()
	for _, msg := range history[client.room] {
		ws.WriteJSON(msg)
	}
	historyMutex.Unlock()

	for {
		var msg Message
		err := ws.ReadJSON(&msg)
		if err != nil {
			roomsMutex.Lock()
			delete(rooms[client.room], client)
			roomsMutex.Unlock()
			break
		}

		if msg.Type == "leave" {
			roomsMutex.Lock()
			delete(rooms[client.room], client)
			roomsMutex.Unlock()
			break
		}

		if msg.Type == "switch" {
			roomsMutex.Lock()
			delete(rooms[client.room], client)
			client.room = msg.Content
			if rooms[client.room] == nil {
				rooms[client.room] = make(map[*Client]bool)
			}
			rooms[client.room][client] = true
			roomsMutex.Unlock()

			historyMutex.Lock()
			for _, hmsg := range history[client.room] {
				client.conn.WriteJSON(hmsg)
			}
			historyMutex.Unlock()
			continue
		}

		broadcast <- msg
	}
}

func handleMessages() {
	for {
		msg := <-broadcast

		historyMutex.Lock()
		history[msg.Room] = append(history[msg.Room], msg)
		historyMutex.Unlock()

		if msg.Type == "vote" {
			votesMutex.Lock()
			if votes[msg.Room] == nil {
				votes[msg.Room] = make(map[string]int)
			}
			votes[msg.Room][msg.Content]++
			result := Message{
				Room:     msg.Room,
				Nickname: "系統",
				Avatar:   "",
				Content:  fmt.Sprintf("投票結果: %v", votes[msg.Room]),
				Type:     "chat",
			}
			for client := range rooms[msg.Room] {
				client.conn.WriteJSON(result)
			}
			votesMutex.Unlock()
			continue
		}

		for client := range rooms[msg.Room] {
			client.conn.WriteJSON(msg)
		}
	}
}
