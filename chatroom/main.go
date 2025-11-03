package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const maxMessageSize = 5 * 1024 * 1024

type Client struct {
	conn *websocket.Conn
	// ✨ (BUG 1) 為每個客戶端加上鎖
	mu       sync.Mutex
	nickname string
	room     string
	avatar   string
}

type Message struct {
	Room       string         `json:"room"`
	Nickname   string         `json:"nickname"`
	Avatar     string         `json:"avatar"`
	Content    string         `json:"content,omitempty"`
	Type       string         `json:"type"`
	Question   string         `json:"question,omitempty"`
	Answer     string         `json:"answer,omitempty"`
	Timestamp  string         `json:"timestamp,omitempty"`
	Emoji      string         `json:"emoji,omitempty"`
	Options    []string       `json:"options,omitempty"`
	Results    map[string]int `json:"results,omitempty"`
	Transcript string         `json:"transcript,omitempty"`
	Tries      int            `json:"tries,omitempty"`
	Time       int            `json:"time,omitempty"`
	X          float64        `json:"x,omitempty"`
	Y          float64        `json:"y,omitempty"`
	Color      string         `json:"color,omitempty"`
	LineWidth  int            `json:"lineWidth,omitempty"`
}

// (Quiz, Vote, GameScore, DrawState Structs 與上一版相同)
type Quiz struct {
	Question string
	Answer   string
	Active   bool
}
type Vote struct {
	Question string
	Options  map[string]int
	Voters   map[string]bool
}
type GameScore struct {
	Nickname string `json:"nickname"`
	Avatar   string `json:"avatar"`
	Tries    int    `json:"tries"`
	Time     int    `json:"time"`
}
type DrawState struct {
	CurrentWord   string
	CurrentDrawer string
}

var (
	rooms     = make(map[string]map[*Client]bool)
	history   = make(map[string][]Message)
	votes     = make(map[string]*Vote)
	quizzes   = make(map[string]*Quiz)
	broadcast = make(chan Message)

	leaderboard      []GameScore
	leaderboardMutex sync.Mutex
	leaderboardFile  = "leaderboard.json"

	drawStates     = make(map[string]*DrawState)
	drawStateMutex sync.Mutex

	roomsMutex   sync.Mutex
	historyMutex sync.Mutex
	votesMutex   sync.Mutex
	quizzesMutex sync.Mutex
)

var upgrader = websocket.Upgrader{
	CheckOrigin:     func(r *http.Request) bool { return true },
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
}

func normalize(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ToLower(s)
	return s
}

func main() {
	loadLeaderboard()
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
		log.Println("Upgrade error:", err)
		return
	}
	defer ws.Close()

	ws.SetReadLimit(maxMessageSize)

	var initMsg Message
	err = ws.ReadJSON(&initMsg)
	if err != nil {
		log.Println("Init message read error:", err)
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

	if !strings.HasPrefix(client.room, "_") {
		broadcastRoomList()
	}

	if !strings.HasPrefix(client.room, "_") {
		historyMutex.Lock()
		if history[client.room] != nil {
			if msgs, ok := history[client.room]; ok && len(msgs) > 0 {
				for _, msg := range msgs {
					// ✨ (BUG 1) 加上鎖
					client.mu.Lock()
					if err := client.conn.WriteJSON(msg); err != nil {
						log.Println("History write error:", err)
						client.mu.Unlock() // 錯誤時也要解鎖
						break
					}
					client.mu.Unlock()
				}
			}
		}
		historyMutex.Unlock()

		joinMsg := Message{
			Type: "join", Room: client.room, Content: client.nickname + " 加入了聊天室", Timestamp: time.Now().Format("15:04"),
		}
		broadcast <- joinMsg
		go broadcastOnlineCount()
	}

	for {
		var msg Message
		err := ws.ReadJSON(&msg)
		if err != nil {
			roomToUpdate := client.room
			roomsMutex.Lock()
			delete(rooms[roomToUpdate], client)
			if len(rooms[roomToUpdate]) == 0 {
				delete(rooms, roomToUpdate)
				if roomToUpdate == "_draw_game_" {
					drawStateMutex.Lock()
					delete(drawStates, roomToUpdate)
					drawStateMutex.Unlock()
				}
			}
			roomsMutex.Unlock()

			if !strings.HasPrefix(roomToUpdate, "_") {
				leaveMsg := Message{
					Type: "leave", Room: roomToUpdate, Content: client.nickname + " 離開了聊天室", Timestamp: time.Now().Format("15:04"),
				}
				broadcast <- leaveMsg
				broadcastRoomList()
				go broadcastOnlineCount()
			}
			break
		}

		if msg.Type == "get_leaderboard" {
			leaderboardMutex.Lock()
			scoresJSON, err := json.Marshal(leaderboard)
			leaderboardMutex.Unlock()
			if err != nil {
				log.Println("Error marshalling leaderboard:", err)
				continue
			}

			resp := Message{Type: "leaderboard_update", Content: string(scoresJSON)}
			// ✨ (BUG 1) 加上鎖
			client.mu.Lock()
			if err := ws.WriteJSON(resp); err != nil {
				log.Println("WriteJSON error (get_leaderboard):", err)
			}
			client.mu.Unlock()
			continue
		}

		if msg.Type == "switch" {
			oldRoom := client.room
			newRoom := msg.Room
			isSwitchingToGame := strings.HasPrefix(newRoom, "_")
			isSwitchingFromGame := strings.HasPrefix(oldRoom, "_")

			if !isSwitchingFromGame {
				leaveMsg := Message{
					Type: "leave", Room: oldRoom, Content: client.nickname + " 離開了聊天室", Timestamp: time.Now().Format("15:04"),
				}
				broadcast <- leaveMsg
			}

			roomsMutex.Lock()
			delete(rooms[oldRoom], client)
			if len(rooms[oldRoom]) == 0 {
				delete(rooms, oldRoom)
			}
			client.room = newRoom
			if rooms[newRoom] == nil {
				rooms[newRoom] = make(map[*Client]bool)
			}
			rooms[newRoom][client] = true
			roomsMutex.Unlock()

			if !isSwitchingFromGame || !isSwitchingToGame {
				broadcastRoomList()
				go broadcastOnlineCount()
			}

			if !isSwitchingToGame {
				historyMutex.Lock()
				if history[client.room] != nil {
					for _, hmsg := range history[client.room] {
						// ✨ (BUG 1) 加上鎖
						client.mu.Lock()
						if err := client.conn.WriteJSON(hmsg); err != nil {
							log.Println("Switch history write error:", err)
							client.mu.Unlock()
							break
						}
						client.mu.Unlock()
					}
				}
				historyMutex.Unlock()

				joinMsg := Message{
					Type: "join", Room: newRoom, Content: client.nickname + " 加入了聊天室", Timestamp: time.Now().Format("15:04"),
				}
				broadcast <- joinMsg
			}
			continue
		}

		if msg.Type == "chat" && msg.Room == "_draw_game_" && strings.HasPrefix(msg.Content, "/setword ") {
			word := strings.TrimSpace(strings.TrimPrefix(msg.Content, "/setword "))
			drawStateMutex.Lock()
			if drawStates[msg.Room] == nil {
				drawStates[msg.Room] = &DrawState{}
			}
			state := drawStates[msg.Room]
			if word != "" && (state.CurrentDrawer == "" || state.CurrentDrawer == client.nickname) {
				state.CurrentWord = word
				state.CurrentDrawer = client.nickname

				// ✨ (BUG 1) 加上鎖
				client.mu.Lock()
				if err := client.conn.WriteJSON(Message{Type: "new_round_drawer", Content: word, Room: msg.Room}); err != nil {
					log.Println("WriteJSON error (draw drawer):", err)
				}
				client.mu.Unlock()

				blanks := strings.Repeat("_ ", len([]rune(word)))
				broadcastMsg := Message{
					Type: "new_round_guesser", Room: msg.Room, Content: blanks, Nickname: client.nickname,
				}
				roomsMutex.Lock()
				for c := range rooms[msg.Room] {
					if c.nickname != client.nickname {
						// ✨ (BUG 1) 加上鎖
						c.mu.Lock()
						if err := c.conn.WriteJSON(broadcastMsg); err != nil {
							log.Println("WriteJSON error (draw guesser):", err)
						}
						c.mu.Unlock()
					}
				}
				roomsMutex.Unlock()
			}
			drawStateMutex.Unlock()
			continue
		}

		if msg.Type == "game_score" {
			msg.Nickname = client.nickname
			msg.Avatar = client.avatar
		}

		msg.Avatar = client.avatar
		broadcast <- msg
	}
}

func broadcastOnlineCount() {
	roomsMutex.Lock()
	allClients := make(map[*Client]bool)
	for roomName, roomClients := range rooms {
		if !strings.HasPrefix(roomName, "_") {
			for client := range roomClients {
				allClients[client] = true
			}
		}
	}
	count := len(allClients)
	roomsMutex.Unlock()

	msg := Message{
		Type:    "online_count",
		Content: fmt.Sprintf("%d", count),
	}

	for client := range allClients {
		// ✨ (BUG 1) 加上鎖
		client.mu.Lock()
		if err := client.conn.WriteJSON(msg); err != nil {
			log.Println("WriteJSON error (online_count):", err)
		}
		client.mu.Unlock()
	}
}

func broadcastRoomList() {
	roomsMutex.Lock()
	var roomNames []string
	for roomName := range rooms {
		if !strings.HasPrefix(roomName, "_") {
			roomNames = append(roomNames, roomName)
		}
	}
	sort.Strings(roomNames)
	foundLobby := false
	for _, r := range roomNames {
		if r == "lobby" {
			foundLobby = true
			break
		}
	}
	if !foundLobby {
		roomNames = append([]string{"lobby"}, roomNames...)
	}
	msg := Message{Type: "room_list", Options: roomNames}
	allClients := make(map[*Client]bool)
	for roomName, room := range rooms {
		if !strings.HasPrefix(roomName, "_") {
			for client := range room {
				allClients[client] = true
			}
		}
	}
	roomsMutex.Unlock() // *在廣播前*解鎖

	for client := range allClients {
		// ✨ (BUG 1) 加上鎖
		client.mu.Lock()
		if err := client.conn.WriteJSON(msg); err != nil {
			log.Println("WriteJSON error (room_list):", err)
		}
		client.mu.Unlock()
	}
}

func handleMessages() {
	for {
		msg := <-broadcast

		if msg.Timestamp == "" {
			msg.Timestamp = time.Now().Format("15:04:05")
		}

		// ✨ (BUG 1) 在所有 client.conn.WriteJSON 處加上鎖
		switch msg.Type {
		case "draw_start", "draw_move", "draw_end", "clear_canvas":
			roomsMutex.Lock()
			for client := range rooms[msg.Room] {
				if client.nickname != msg.Nickname {
					client.mu.Lock()
					if err := client.conn.WriteJSON(msg); err != nil {
						log.Println("WriteJSON error (draw):", err)
					}
					client.mu.Unlock()
				}
			}
			roomsMutex.Unlock()
			continue

		case "game_score":
			leaderboardMutex.Lock()
			newScore := GameScore{
				Nickname: msg.Nickname, Avatar: msg.Avatar, Tries: msg.Tries, Time: msg.Time,
			}
			leaderboard = append(leaderboard, newScore)
			sort.Slice(leaderboard, func(i, j int) bool {
				if leaderboard[i].Tries != leaderboard[j].Tries {
					return leaderboard[i].Tries < leaderboard[j].Tries
				}
				return leaderboard[i].Time < leaderboard[j].Time
			})
			if len(leaderboard) > 10 {
				leaderboard = leaderboard[:10]
			}
			leaderboardMutex.Unlock()
			saveLeaderboard()
			broadcastLeaderboard()
			announceMsg := Message{
				Type: "chat", Room: "lobby", Nickname: "🏆 系統", Avatar: "🏆",
				Content:   fmt.Sprintf("%s 在猜數字遊戲中獲勝了 (猜 %d 次, %d 秒)！", msg.Nickname, msg.Tries, msg.Time),
				Timestamp: time.Now().Format("15:04:05"),
			}
			broadcast <- announceMsg
			continue

		case "reaction":
			roomsMutex.Lock()
			for client := range rooms[msg.Room] {
				client.mu.Lock()
				if err := client.conn.WriteJSON(msg); err != nil {
					log.Println("WriteJSON error (reaction):", err)
				}
				client.mu.Unlock()
			}
			roomsMutex.Unlock()
			continue

		case "vote":
			votesMutex.Lock()
			optionsMap := make(map[string]int)
			for _, opt := range msg.Options {
				optionsMap[opt] = 0
			}
			votes[msg.Room] = &Vote{
				Question: msg.Question, Options: optionsMap, Voters: make(map[string]bool),
			}
			votesMutex.Unlock()
			historyMutex.Lock()
			history[msg.Room] = append(history[msg.Room], msg)
			historyMutex.Unlock()
			roomsMutex.Lock()
			for client := range rooms[msg.Room] {
				client.mu.Lock()
				if err := client.conn.WriteJSON(msg); err != nil {
					log.Println("WriteJSON error (vote start):", err)
				}
				client.mu.Unlock()
			}
			roomsMutex.Unlock()
			continue

		case "vote_answer":
			votesMutex.Lock()
			currentVote, exists := votes[msg.Room]
			var resultMsg Message
			if exists && !currentVote.Voters[msg.Nickname] {
				if _, ok := currentVote.Options[msg.Answer]; ok {
					currentVote.Options[msg.Answer]++
					currentVote.Voters[msg.Nickname] = true
				}
				resultMsg = Message{
					Type: "vote_result", Room: msg.Room, Content: msg.Content, Results: currentVote.Options,
				}
			}
			votesMutex.Unlock()
			if resultMsg.Type != "" {
				roomsMutex.Lock()
				for client := range rooms[msg.Room] {
					client.mu.Lock()
					if err := client.conn.WriteJSON(resultMsg); err != nil {
						log.Println("WriteJSON error (vote result):", err)
					}
					client.mu.Unlock()
				}
				roomsMutex.Unlock()
			}
			continue

		case "quiz_start":
			quizzesMutex.Lock()
			quizzes[msg.Room] = &Quiz{
				Question: msg.Question, Answer: msg.Answer, Active: true,
			}
			quizzesMutex.Unlock()
			broadcastMsg := Message{
				Type: "quiz_start", Room: msg.Room, Nickname: msg.Nickname, Avatar: msg.Avatar,
				Question: msg.Question, Timestamp: msg.Timestamp,
			}
			historyMutex.Lock()
			history[msg.Room] = append(history[msg.Room], broadcastMsg)
			historyMutex.Unlock()
			roomsMutex.Lock()
			for client := range rooms[msg.Room] {
				client.mu.Lock()
				if err := client.conn.WriteJSON(broadcastMsg); err != nil {
					log.Println("WriteJSON error (quiz start):", err)
				}
				client.mu.Unlock()
			}
			roomsMutex.Unlock()
			continue

		case "quiz_answer":
			quizzesMutex.Lock()
			quiz, exists := quizzes[msg.Room]
			isCorrect := exists && quiz.Active && (quiz.Answer == msg.Answer)
			var correctAnswer string
			if isCorrect {
				quiz.Active = false
			}
			if exists {
				correctAnswer = quiz.Answer
			}
			quizzesMutex.Unlock()
			if isCorrect {
				resultMsg := Message{
					Type: "quiz_result", Room: msg.Room, Nickname: msg.Nickname, Avatar: msg.Avatar,
					Content: msg.Content, Answer: correctAnswer, Timestamp: time.Now().Format("15:04:05"),
				}
				historyMutex.Lock()
				history[msg.Room] = append(history[msg.Room], resultMsg)
				historyMutex.Unlock()
				roomsMutex.Lock()
				for client := range rooms[msg.Room] {
					client.mu.Lock()
					if err := client.conn.WriteJSON(resultMsg); err != nil {
						log.Println("WriteJSON error (quiz result):", err)
					}
					client.mu.Unlock()
				}
				roomsMutex.Unlock()
			}
			continue

		case "chat":
			if msg.Room == "_draw_game_" {
				drawStateMutex.Lock()
				if drawStates[msg.Room] == nil {
					drawStates[msg.Room] = &DrawState{}
				}
				state := drawStates[msg.Room]
				if state.CurrentWord != "" && msg.Nickname != state.CurrentDrawer && normalize(msg.Content) == normalize(state.CurrentWord) {
					broadcastMsg := Message{
						Type: "guess_correct", Room: msg.Room, Nickname: msg.Nickname,
						Content: state.CurrentWord, Timestamp: time.Now().Format("15:04:05"),
					}
					roomsMutex.Lock()
					for client := range rooms[msg.Room] {
						client.mu.Lock()
						if err := client.conn.WriteJSON(broadcastMsg); err != nil {
							log.Println("WriteJSON error (draw correct):", err)
						}
						client.mu.Unlock()
					}
					roomsMutex.Unlock()
					state.CurrentWord = ""
					state.CurrentDrawer = ""
					drawStateMutex.Unlock()
					continue
				}
				drawStateMutex.Unlock()
			}
			historyMutex.Lock()
			if !strings.HasPrefix(msg.Room, "_") {
				if len(history[msg.Room]) > 100 {
					history[msg.Room] = history[msg.Room][len(history[msg.Room])-100:]
				}
				history[msg.Room] = append(history[msg.Room], msg)
			}
			historyMutex.Unlock()
			roomsMutex.Lock()
			for client := range rooms[msg.Room] {
				client.mu.Lock()
				if err := client.conn.WriteJSON(msg); err != nil {
					log.Println("WriteJSON error (chat broadcast):", err)
				}
				client.mu.Unlock()
			}
			roomsMutex.Unlock()
			continue

		default: // image, voice, join, leave
			historyMutex.Lock()
			if !strings.HasPrefix(msg.Room, "_") {
				if msg.Type == "image" || msg.Type == "voice" || msg.Content != "" {
					if len(history[msg.Room]) > 100 {
						history[msg.Room] = history[msg.Room][len(history[msg.Room])-100:]
					}
					history[msg.Room] = append(history[msg.Room], msg)
				}
			}
			historyMutex.Unlock()
			roomsMutex.Lock()
			for client := range rooms[msg.Room] {
				client.mu.Lock()
				if err := client.conn.WriteJSON(msg); err != nil {
					log.Println("WriteJSON error (default broadcast):", err)
				}
				client.mu.Unlock()
			}
			roomsMutex.Unlock()
		}
	}
}

// (loadLeaderboard, saveLeaderboard, broadcastLeaderboard 與上一版相同)
func loadLeaderboard() {
	leaderboardMutex.Lock()
	defer leaderboardMutex.Unlock()
	file, err := os.ReadFile(leaderboardFile)
	if err != nil {
		log.Println("No leaderboard file found, starting fresh.")
		leaderboard = make([]GameScore, 0)
		return
	}
	err = json.Unmarshal(file, &leaderboard)
	if err != nil {
		log.Println("Error parsing leaderboard file:", err)
		leaderboard = make([]GameScore, 0)
	}
}

func saveLeaderboard() {
	leaderboardMutex.Lock()
	defer leaderboardMutex.Unlock()
	file, err := json.MarshalIndent(leaderboard, "", "  ")
	if err != nil {
		log.Println("Error marshalling leaderboard:", err)
		return
	}
	err = os.WriteFile(leaderboardFile, file, 0644)
	if err != nil {
		log.Println("Error saving leaderboard file:", err)
	}
}

func broadcastLeaderboard() {
	leaderboardMutex.Lock()
	scoresJSON, err := json.Marshal(leaderboard)
	leaderboardMutex.Unlock()
	if err != nil {
		log.Println("Error marshalling leaderboard broadcast:", err)
		return
	}
	msg := Message{Type: "leaderboard_update", Content: string(scoresJSON)}
	roomsMutex.Lock()
	gameClients, ok := rooms["_game_"]
	if ok {
		for client := range gameClients {
			// ✨ (BUG 1) 加上鎖
			client.mu.Lock()
			if err := client.conn.WriteJSON(msg); err != nil {
				log.Println("WriteJSON error (leaderboard):", err)
			}
			client.mu.Unlock()
		}
	}
	roomsMutex.Unlock()
}
