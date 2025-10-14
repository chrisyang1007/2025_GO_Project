package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Client struct {
	conn     *websocket.Conn
	nickname string
	room     string
	avatar   string
}

type Message struct {
	Room      string `json:"room"`
	Nickname  string `json:"nickname"`
	Avatar    string `json:"avatar"`
	Content   string `json:"content,omitempty"`
	Type      string `json:"type"` // chat, vote, leave, switch
	Question  string `json:"question,omitempty"`
	Answer    string `json:"answer,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
}

type Quiz struct {
	Question string
	Answer   string
	Active   bool
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

var (
	rooms        = make(map[string]map[*Client]bool)
	history      = make(map[string][]Message)
	votes        = make(map[string]map[string]int)
	quizzes      = make(map[string]*Quiz)
	broadcast    = make(chan Message)
	roomsMutex   sync.Mutex
	historyMutex sync.Mutex
	votesMutex   sync.Mutex
	quizzesMutex sync.Mutex
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

	broadcastUserList(client.room)

	historyMutex.Lock()
	for _, msg := range history[client.room] {
		ws.WriteJSON(msg)
	}
	historyMutex.Unlock()

	for {
		var msg Message
		err := ws.ReadJSON(&msg)
		if err != nil {
			roomToUpdate := client.room
			roomsMutex.Lock()
			delete(rooms[roomToUpdate], client)
			roomsMutex.Unlock()
			broadcastUserList(roomToUpdate)
			break
		}

		if msg.Type == "leave" {
			roomToUpdate := client.room
			roomsMutex.Lock()
			delete(rooms[roomToUpdate], client)
			roomsMutex.Unlock()
			broadcastUserList(roomToUpdate)
			break
		}

		if msg.Type == "switch" {
			oldRoom := client.room
			newRoom := msg.Content

			roomsMutex.Lock()
			delete(rooms[oldRoom], client)
			client.room = newRoom
			if rooms[newRoom] == nil {
				rooms[newRoom] = make(map[*Client]bool)
			}
			rooms[newRoom][client] = true
			roomsMutex.Unlock()

			broadcastUserList(oldRoom)
			broadcastUserList(newRoom)

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

func broadcastUserList(room string) {
	roomsMutex.Lock()
	defer roomsMutex.Unlock()

	var users []string
	for client := range rooms[room] {
		users = append(users, client.nickname)
	}
	sort.Strings(users)

	userListJSON, err := json.Marshal(users)
	if err != nil {
		log.Println("Error marshalling user list:", err)
		return
	}

	msg := Message{
		Type:    "user_list",
		Room:    room,
		Content: string(userListJSON),
	}

	for client := range rooms[room] {
		if err := client.conn.WriteJSON(msg); err != nil {
			log.Println("WriteJSON error:", err)
		}
	}
}

func handleMessages() {
	for {
		msg := <-broadcast

		if msg.Type != "typing_start" && msg.Type != "typing_stop" {
			msg.Timestamp = time.Now().Format("15:04")
		}

		switch msg.Type {
		case "typing_start", "typing_stop":
			roomsMutex.Lock()
			for client := range rooms[msg.Room] {
				if client.nickname != msg.Nickname {
					if err := client.conn.WriteJSON(msg); err != nil {
						log.Println("WriteJSON error (typing):", err)
					}
				}
			}
			roomsMutex.Unlock()
			continue
		case "vote":
			votesMutex.Lock()
			if votes[msg.Room] == nil {
				votes[msg.Room] = make(map[string]int)
			}
			votes[msg.Room][msg.Content]++
			result := Message{
				Room:      msg.Room,
				Nickname:  "系統",
				Avatar:    "",
				Content:   fmt.Sprintf("投票結果: %v", votes[msg.Room]),
				Type:      "chat",
				Timestamp: time.Now().Format("15:04"),
			}
			historyMutex.Lock()
			history[msg.Room] = append(history[msg.Room], msg)
			historyMutex.Unlock()

			for client := range rooms[msg.Room] {
				client.conn.WriteJSON(result)
			}
			votesMutex.Unlock()
			continue

		case "quiz_start":
			quizzesMutex.Lock()
			quizzes[msg.Room] = &Quiz{
				Question: msg.Question,
				Answer:   msg.Answer,
				Active:   true,
			}
			quizzesMutex.Unlock()

			broadcastMsg := Message{
				Type:      "quiz_start",
				Room:      msg.Room,
				Nickname:  msg.Nickname,
				Question:  msg.Question,
				Timestamp: msg.Timestamp,
			}

			historyMutex.Lock()
			history[msg.Room] = append(history[msg.Room], broadcastMsg)
			historyMutex.Unlock()

			for client := range rooms[msg.Room] {
				client.conn.WriteJSON(broadcastMsg)
			}
			continue

		case "quiz_answer":
			quizzesMutex.Lock()
			quiz, exists := quizzes[msg.Room]
			isCorrect := exists && quiz.Active && (quiz.Answer == msg.Answer)
			if isCorrect {
				quiz.Active = false
			}
			quizzesMutex.Unlock()

			if isCorrect {
				resultMsg := Message{
					Type:      "quiz_result",
					Room:      msg.Room,
					Nickname:  msg.Nickname,
					Answer:    quiz.Answer,
					Timestamp: time.Now().Format("15:04"),
				}
				for client := range rooms[msg.Room] {
					client.conn.WriteJSON(resultMsg)
				}
			}
			continue

		default:
			historyMutex.Lock()
			history[msg.Room] = append(history[msg.Room], msg)
			historyMutex.Unlock()
		}

		for client := range rooms[msg.Room] {
			client.conn.WriteJSON(msg)
		}
	}
}
