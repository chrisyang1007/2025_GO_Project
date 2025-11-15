package service

import (
	"chatroom/models"
	"log"
	"strings"
	"time"
)

// HandleMessageLoop (
func (s *StateService) HandleMessageLoop() {
	for {
		msg := <-s.Broadcast

		if msg.Timestamp == "" {
			msg.Timestamp = time.Now().Format("15:04:05")
		}

		s.processMessage(msg)
	}
}

func normalize(str string) string {
	return strings.ToLower(strings.TrimSpace(str))
}

// processMessage
func (s *StateService) processMessage(msg models.Message) {
	//
	log.Printf("[DEBUG] Processing Message: Type=[%s], Room=[%s], Nick=[%s]", msg.Type, msg.Room, msg.Nickname)

	switch msg.Type {
	case "draw_start", "draw_move", "draw_end", "clear_canvas":
		s.handleDraw(msg)
	case "game_score":
		s.handleGameScore(msg)
	case "reaction":
		s.handleReaction(msg)
	case "vote":
		s.handleVoteStart(msg)
	case "vote_answer":
		s.handleVoteAnswer(msg)
	case "quiz_start":
		s.handleQuizStart(msg)
	case "quiz_answer":
		s.handleQuizAnswer(msg)
	case "chat":
		s.handleChat(msg)
	default: // image, voice, join, leave
		s.handleDefault(msg)
	}
}

// handleDraw
func (s *StateService) handleDraw(msg models.Message) {
	var clientsToWrite []*models.Client

	s.RoomsMutex.RLock()
	clientsInRoom, ok := s.Rooms[msg.Room]
	if ok {
		for client := range clientsInRoom {
			if client.Nickname != msg.Nickname {
				clientsToWrite = append(clientsToWrite, client)
			}
		}
	}
	s.RoomsMutex.RUnlock()

	for _, client := range clientsToWrite {
		client.Mu.Lock()
		if err := client.Conn.WriteJSON(msg); err != nil {
			log.Println("WriteJSON error (draw broadcast):", err)
		}
		client.Mu.Unlock()
	}
}

// handleGameScore
func (s *StateService) handleGameScore(msg models.Message) {
	newScore := models.GameScore{
		Nickname: msg.Nickname, Avatar: msg.Avatar, Tries: msg.Tries, Time: msg.Time,
	}
	s.updateLeaderboard(newScore)
}

// handleReaction
func (s *StateService) handleReaction(msg models.Message) {
	s.BroadcastToRoom(msg)
}

// handleVoteStart
func (s *StateService) handleVoteStart(msg models.Message) {
	s.VotesMutex.Lock()
	optionsMap := make(map[string]int)
	for _, opt := range msg.Options {
		optionsMap[opt] = 0
	}
	s.Votes[msg.Room] = &models.Vote{
		Question: msg.Question, Options: optionsMap, Voters: make(map[string]bool),
	}
	s.VotesMutex.Unlock()

	s.HistoryMutex.Lock()
	s.History[msg.Room] = append(s.History[msg.Room], msg)
	s.HistoryMutex.Unlock()

	s.BroadcastToRoom(msg)
}

// handleVoteAnswer
func (s *StateService) handleVoteAnswer(msg models.Message) {
	s.VotesMutex.Lock()
	currentVote, exists := s.Votes[msg.Room]
	var resultMsg models.Message
	if exists && !currentVote.Voters[msg.Nickname] {
		if _, ok := currentVote.Options[msg.Answer]; ok {
			currentVote.Options[msg.Answer]++
			currentVote.Voters[msg.Nickname] = true
		}
		resultMsg = models.Message{
			Type: "vote_result", Room: msg.Room, Content: msg.Content, Results: currentVote.Options,
		}
	}
	s.VotesMutex.Unlock()

	if resultMsg.Type != "" {
		s.BroadcastToRoom(resultMsg)
	}
}

// handleQuizStart
func (s *StateService) handleQuizStart(msg models.Message) {
	s.QuizzesMutex.Lock()
	s.Quizzes[msg.Room] = &models.Quiz{
		Question: msg.Question, Answer: msg.Answer, Active: true,
	}
	s.QuizzesMutex.Unlock()

	broadcastMsg := models.Message{
		Type: "quiz_start", Room: msg.Room, Nickname: msg.Nickname, Avatar: msg.Avatar,
		Question: msg.Question, Timestamp: msg.Timestamp,
	}

	s.HistoryMutex.Lock()
	s.History[msg.Room] = append(s.History[msg.Room], broadcastMsg)
	s.HistoryMutex.Unlock()

	s.BroadcastToRoom(broadcastMsg)
}

// handleQuizAnswer
func (s *StateService) handleQuizAnswer(msg models.Message) {
	s.QuizzesMutex.Lock()
	quiz, exists := s.Quizzes[msg.Room]
	isCorrect := exists && quiz.Active && (quiz.Answer == msg.Answer)
	var correctAnswer string
	if isCorrect {
		quiz.Active = false
	}
	if exists {
		correctAnswer = quiz.Answer
	}
	s.QuizzesMutex.Unlock()

	if isCorrect {
		resultMsg := models.Message{
			Type: "quiz_result", Room: msg.Room, Nickname: msg.Nickname, Avatar: msg.Avatar,
			Content: msg.Content, Answer: correctAnswer, Timestamp: time.Now().Format("15:04:05"),
		}
		s.HistoryMutex.Lock()
		s.History[msg.Room] = append(s.History[msg.Room], resultMsg)
		s.HistoryMutex.Unlock()
		s.BroadcastToRoom(resultMsg)
	}
}

// handleChat
func (s *StateService) handleChat(msg models.Message) {
	//
	if msg.Room == "_draw_game_" {
		s.DrawStateMutex.Lock()
		if s.DrawStates[msg.Room] == nil {
			s.DrawStates[msg.Room] = &models.DrawState{}
		}
		state := s.DrawStates[msg.Room]
		if state.CurrentWord != "" && msg.Nickname != state.CurrentDrawer && normalize(msg.Content) == normalize(state.CurrentWord) {
			broadcastMsg := models.Message{
				Type: "guess_correct", Room: msg.Room, Nickname: msg.Nickname,
				Content: state.CurrentWord, Timestamp: time.Now().Format("15:04:05"),
			}
			s.BroadcastToRoom(broadcastMsg)
			state.CurrentWord = ""
			state.CurrentDrawer = ""
			s.DrawStateMutex.Unlock()
			return
		}
		s.DrawStateMutex.Unlock()
	}

	s.HistoryMutex.Lock()
	if !strings.HasPrefix(msg.Room, "_") {
		if len(s.History[msg.Room]) > 100 {
			s.History[msg.Room] = s.History[msg.Room][len(s.History[msg.Room])-100:]
		}
		s.History[msg.Room] = append(s.History[msg.Room], msg)
	}
	s.HistoryMutex.Unlock()

	s.BroadcastToRoom(msg)
}

// handleDefault
func (s *StateService) handleDefault(msg models.Message) {
	s.HistoryMutex.Lock()
	if !strings.HasPrefix(msg.Room, "_") {
		if msg.Type == "image" || msg.Type == "voice" || msg.Content != "" {
			if len(s.History[msg.Room]) > 100 {
				s.History[msg.Room] = s.History[msg.Room][len(s.History[msg.Room])-100:]
			}
			s.History[msg.Room] = append(s.History[msg.Room], msg)
		}
	}
	s.HistoryMutex.Unlock()

	s.BroadcastToRoom(msg)
}
