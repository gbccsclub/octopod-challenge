package internal

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

var UpdateInterval = 1 * 15 * time.Second
var TimeoutInterval = 1 * time.Second
var MaxInactive = 2

type Lobby struct {
	DiscordBot   *DiscordBot
	Maze         *Maze
	Octapods     map[string]*Octapod
	Mutex        sync.RWMutex
	timerRunning bool
}

type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

func NewLobby(width, height int) *Lobby {
	maze := NewMaze(width, height)
	maze.Generate()

	bot := NewDiscordBot()

	lobby := &Lobby{
		DiscordBot: bot,
		Maze:       maze,
		Octapods:   make(map[string]*Octapod),
	}

	bot.SetLobby(lobby)

	fmt.Println(maze.Print())
	lobby.StartTimer(TimeoutInterval)
	return lobby
}

func (l *Lobby) StartTimer(timeout time.Duration) {
	l.Mutex.Lock()
	if l.timerRunning {
		l.Mutex.Unlock()
		log.Println("Timer already running.")
		return
	}
	l.timerRunning = true
	l.Mutex.Unlock()

	duration := UpdateInterval

	go func() {
		t := duration
		isTimeout := false
		for {
			timer := time.NewTimer(t)
			<-timer.C
			if !isTimeout {
				l.Update()
				log.Println("Sensor data pinged (Update)")
				t = timeout
			} else {
				l.TimeoutUpdate()
				log.Println("Timeout update")
				l.DiscordBot.SendMessage("Board updated:\n" + l.DisplayMaze(""))
				t = duration
			}
			isTimeout = !isTimeout
		}
	}()
}

func (l *Lobby) DisplayMaze(id string) string {
	l.Mutex.Lock()
	defer l.Mutex.Unlock()

	octapodPositions := make(map[Point]*Octapod)

	for _, o := range l.Octapods {
		if id == "" {
			octapodPositions[Point{
				int(o.Position.X()),
				int(o.Position.Y()),
			}] = o
		} else if o.Id == strings.ToLower(id) {
			octapodPositions[Point{
				int(o.Position.X()),
				int(o.Position.Y()),
			}] = o
		}
	}

	if len(octapodPositions) == 0 {
		if id == "" {
			return "No octapods in the lobby."
		} else {
			return "No octapods [" + id + "] in the lobby."
		}
	}

	var result string
	for y := 0; y < l.Maze.Height; y++ {
		for x := 0; x < l.Maze.Width; x++ {
			if l.Maze.cells[x][y] {
				result += "# " // Wall
			} else if octapod, exists := octapodPositions[Point{x, y}]; exists {
				result += octapod.Id[0:1] + " "
			} else {
				result += "  "
			}
		}
		result += "# \n"
	}
	for x := 0; x < l.Maze.Width; x++ {
		result += "# "
	}
	result += "# \n"
	return "```\n" + result + "```"
}

func (l *Lobby) HandleJoin(c *gin.Context) {
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		log.Println(err)
		return
	}
	log.Println("New connection established")

	err, auth := l.getAuthenticationMessage(conn)
	if err != nil {
		log.Println(err)
		return
	}
	log.Println("Received an octapod authentication message")

	o := l.identifyOctapod(strings.ToLower(auth.ID), auth.Password, conn)
	if o == nil {
		return
	}
	o.Run()
}

func (l *Lobby) getAuthenticationMessage(conn *websocket.Conn) (error, *AuthMessage) {
	msgType, content, err := conn.ReadMessage()
	if err != nil {
		sendErrorAndClose(conn, "Error reading authentication message: "+err.Error())
		return err, nil
	}
	if msgType != websocket.TextMessage {
		sendErrorAndClose(conn, "Authentication requires a text message with credentials.")
		return errors.New("non-text auth message"), nil
	}
	var auth AuthMessage
	if err := json.Unmarshal(content, &auth); err != nil {
		sendErrorAndClose(conn, "Invalid authentication message format.")
		return err, nil
	}
	return nil, &auth
}

func (l *Lobby) identifyOctapod(id, password string, conn *websocket.Conn) *Octapod {
	l.Mutex.Lock()
	oct, exists := l.Octapods[id]
	if !exists {
		oct = NewOctapod(id, password, conn, l.Maze)
		l.Octapods[id] = oct
		l.Mutex.Unlock()
		log.Println("New octapod [", id, "] registered")
		l.DiscordBot.SendMessage("New octapod [" + id + "] registered")
		oct.Run()
		return oct
	}
	// existing
	l.Mutex.Unlock()

	// verify and reconnect under octapod's lock
	oct.Mutex.Lock()
	defer oct.Mutex.Unlock()
	if !oct.VerifyPassword(password) {
		sendErrorAndClose(conn, "Invalid password for octapod")
		conn.Close()
		return oct
	}
	if oct.Conn != nil {
		sendErrorAndClose(conn, "Octapod already connected")
		conn.Close()
		return oct
	}
	oct.Conn = conn
	log.Println("Octapod [", id, "] reconnected")
	l.DiscordBot.SendMessage("Octapod [" + id + "] reconnected")
	return oct
}

func (l *Lobby) Update() {
	l.Mutex.RLock()
	pods := make([]*Octapod, 0, len(l.Octapods))
	for _, o := range l.Octapods {
		pods = append(pods, o)
	}
	l.Mutex.RUnlock()

	for _, o := range pods {
		o.Mutex.Lock()
		if o.Conn == nil {
			o.Mutex.Unlock()
			continue
		}
		o.InactiveCount++
		s := l.Maze.GetSensor(o.Position)
		o.Mutex.Unlock()

		o.Sensor <- s
		log.Println("Sensor data sent to octapod [", o.Id, "]")
	}
}

func (l *Lobby) TimeoutUpdate() {
	l.Mutex.RLock()
	pods := make([]*Octapod, 0, len(l.Octapods))
	for _, o := range l.Octapods {
		pods = append(pods, o)
	}
	l.Mutex.RUnlock()

	for _, o := range pods {
		o.Mutex.Lock()
		if o.Conn == nil {
			o.Mutex.Unlock()
			continue
		}
		o.Mutex.Unlock()

		o.Sensor <- nil
		log.Println("Timeout signal sent to octapod", o.Id)

		o.Mutex.Lock()
		if o.InactiveCount >= MaxInactive {
			o.Mutex.Unlock()
			o.Disconnect()
			log.Println("Octapod [", o.Id, "] disconnected due to inactivity")
		} else {
			o.Mutex.Unlock()
		}
	}
}

func sendErrorAndClose(conn *websocket.Conn, msg string) {
	errMsg := ErrorMessage{Error: msg}
	b, _ := json.Marshal(errMsg)
	err := conn.WriteMessage(websocket.TextMessage, b)
	if err != nil {
		log.Println("Error sending error message:", err)
		return
	}
	err = conn.Close()
	if err != nil {
		log.Println("Error closing connection:", err)
		return
	}
}
