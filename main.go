package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for development
	},
}

// Client represents a connected user in a room
type Client struct {
	ID       string `json:"id"`
	UserName string `json:"userName"`
	Conn     *websocket.Conn
	Send     chan []byte
	Room     *Room
}

// Room represents a group of clients associated with a templateId or versionId
type Room struct {
	ID      string
	Clients map[*Client]bool
	Mu      sync.Mutex
}

// Hub manages rooms and client registration
type Hub struct {
	Rooms map[string]*Room
	Mu    sync.Mutex
}

var hub = &Hub{
	Rooms: make(map[string]*Room),
}

func (h *Hub) GetOrCreateRoom(roomID string) *Room {
	h.Mu.Lock()
	defer h.Mu.Unlock()

	if room, ok := h.Rooms[roomID]; ok {
		return room
	}

	room := &Room{
		ID:      roomID,
		Clients: make(map[*Client]bool),
	}
	h.Rooms[roomID] = room
	log.Printf("Created new room: %s", roomID)
	return room
}

func (r *Room) Register(c *Client) {
	r.Mu.Lock()
	r.Clients[c] = true
	r.Mu.Unlock()
	log.Printf("Client %s registered to room %s. Total clients: %d", c.ID, r.ID, len(r.Clients))
	r.BroadcastUserList()
}

func (r *Room) Unregister(c *Client) {
	r.Mu.Lock()
	if _, ok := r.Clients[c]; ok {
		delete(r.Clients, c)
		log.Printf("Client %s unregistered from room %s. Total clients: %d", c.ID, r.ID, len(r.Clients))
	}
	r.Mu.Unlock()
	r.BroadcastUserList()
}

func (r *Room) BroadcastUserList() {
	r.Mu.Lock()
	defer r.Mu.Unlock()

	users := make([]map[string]string, 0)
	for client := range r.Clients {
		users = append(users, map[string]string{
			"id":       client.ID,
			"userName": client.UserName,
		})
	}

	msg, _ := json.Marshal(map[string]interface{}{
		"type":  "user_list",
		"users": users,
	})

	for client := range r.Clients {
		select {
		case client.Send <- msg:
		default:
			close(client.Send)
			delete(r.Clients, client)
		}
	}
}

func (r *Room) Broadcast(message []byte, ignore *Client) {
	r.Mu.Lock()
	defer r.Mu.Unlock()
	for client := range r.Clients {
		if client != ignore {
			select {
			case client.Send <- message:
			default:
				close(client.Send)
				delete(r.Clients, client)
			}
		}
	}
}

func (c *Client) ReadPump() {
	defer func() {
		c.Room.Unregister(c)
		c.Conn.Close()
	}()

	for {
		_, message, err := c.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("error: %v", err)
			}
			break
		}

		// Try to parse message to see if it needs enhancement
		var msgData map[string]interface{}
		if err := json.Unmarshal(message, &msgData); err == nil {
			msgData["senderId"] = c.ID
			msgData["senderName"] = c.UserName
			enhancedMsg, _ := json.Marshal(msgData)
			c.Room.Broadcast(enhancedMsg, c)
		} else {
			// If not JSON, just broadcast as is
			c.Room.Broadcast(message, c)
		}
	}
}

func (c *Client) WritePump() {
	defer func() {
		c.Conn.Close()
	}()

	for {
		message, ok := <-c.Send
		if !ok {
			c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
			return
		}

		err := c.Conn.WriteMessage(websocket.TextMessage, message)
		if err != nil {
			return
		}
	}
}

type InitMessage struct {
	TemplateID string `json:"templateId"`
	VersionID  string `json:"versionId"`
}

func serveWs(w http.ResponseWriter, r *http.Request) {
	// Try to get ID from query parameters first
	roomID := r.URL.Query().Get("templateId")
	if roomID == "" {
		roomID = r.URL.Query().Get("versionId")
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}

	// If ID not in query params, wait for initial message
	if roomID == "" {
		_, p, err := conn.ReadMessage()
		if err != nil {
			log.Println("Initial message error:", err)
			conn.Close()
			return
		}

		var init InitMessage
		if err := json.Unmarshal(p, &init); err != nil {
			log.Println("Invalid initial message format, closing.")
			conn.Close()
			return
		}

		roomID = init.TemplateID
		if roomID == "" {
			roomID = init.VersionID
		}
	}

	if roomID == "" {
		log.Println("No ID provided, closing connection.")
		conn.Close()
		return
	}

	room := hub.GetOrCreateRoom(roomID)

	// Generate a simple ID for the client
	clientID := fmt.Sprintf("user_%d", r.Context().Value(0)) // simplified for demo
	if cid := r.URL.Query().Get("userId"); cid != "" {
		clientID = cid
	} else {
		clientID = fmt.Sprintf("user_%d", SystemRandomID())
	}

	userName := r.URL.Query().Get("userName")
	if userName == "" {
		userName = clientID
	}

	client := &Client{
		ID:       clientID,
		UserName: userName,
		Conn:     conn,
		Send:     make(chan []byte, 256),
		Room:     room,
	}

	room.Register(client)

	go client.WritePump()
	go client.ReadPump()

	// Notify client that they joined
	joinMsg := map[string]interface{}{
		"type":     "system",
		"status":   "connected",
		"room":     roomID,
		"userId":   clientID,
		"userName": userName,
		"message":  "Welcome to room " + roomID,
	}
	resp, _ := json.Marshal(joinMsg)
	client.Send <- resp
}

func SystemRandomID() int {
	return int(time.Now().UnixNano() % 10000)
}

func main() {
	http.HandleFunc("/ws", serveWs)

	port := "8080"
	fmt.Printf("Socket server started on :%s\n", port)
	err := http.ListenAndServe(":"+port, nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}
