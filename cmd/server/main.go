package main

import (
    "encoding/json"
    "io"
    "log"
    "math/rand"
    "mime/multipart"
    "net"
    "net/http"
    "os"
    "path/filepath"
    "strings"
    "sync"
    "time"

    "github.com/gorilla/handlers"
    "github.com/gorilla/mux"
    "github.com/gorilla/websocket"
)

/* -------------------------------------------------------------------------- */
/*                              ABSOLUTE UPLOAD DIR                            */
/* -------------------------------------------------------------------------- */

var uploadDir = "/home/ubuntu/projects/sync-play/uploads"

/* -------------------------------------------------------------------------- */
/*                              WebSocket Upgrader                             */
/* -------------------------------------------------------------------------- */

var upgrader = websocket.Upgrader{
    CheckOrigin: func(r *http.Request) bool { return true },
}

/* -------------------------------------------------------------------------- */
/*                                TYPES & STATE                                */
/* -------------------------------------------------------------------------- */

type Client struct {
    conn *websocket.Conn
    role string
}

type Room struct {
    sync.Mutex
    clients      map[*Client]bool
    host         *Client
    videoPath    string
    subtitlePath string
}

var rooms = make(map[string]*Room)
var roomsMutex = sync.Mutex{}

type Message struct {
    Type        string          `json:"type"`
    Role        string          `json:"role,omitempty"`
    Room        string          `json:"room,omitempty"`
    SDP         json.RawMessage `json:"sdp,omitempty"`
    Candidate   json.RawMessage `json:"candidate,omitempty"`
    Action      string          `json:"action,omitempty"`
    Time        float64         `json:"time,omitempty"`
    VideoURL    string          `json:"videoURL,omitempty"`
    SubtitleURL string          `json:"subtitleURL,omitempty"`
    Label       string          `json:"label,omitempty"`
    Srclang     string          `json:"srclang,omitempty"`
}

/* -------------------------------------------------------------------------- */
/*                                    MAIN                                     */
/* -------------------------------------------------------------------------- */

func main() {
    rand.Seed(time.Now().UnixNano())

    r := mux.NewRouter()

    r.HandleFunc("/ws", handleWebSocket)
    r.HandleFunc("/api/create-room", createRoomHandler).Methods("POST")
    r.HandleFunc("/api/upload/{roomId}", uploadHandler).Methods("POST")
    r.HandleFunc("/api/upload-subtitle/{roomId}", subtitleUploadHandler).Methods("POST")

    /* ---------------------------------------------------------------------- */
    /*                     FIXED STATIC FILE SERVING (B)                      */
    /* ---------------------------------------------------------------------- */

    r.PathPrefix("/uploads/").Handler(http.StripPrefix("/uploads/",
        http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            clean := strings.TrimPrefix(r.URL.Path, "/")
            filePath := filepath.Join(uploadDir, clean)

            if strings.HasSuffix(filePath, ".vtt") {
                w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
            }

            log.Println("SERVE:", filePath)
            http.ServeFile(w, r, filePath)
        }),
    ))

    // CORS
    corsHandler := handlers.CORS(
        handlers.AllowedOrigins([]string{"*"}),
        handlers.AllowedMethods([]string{"GET", "POST", "OPTIONS"}),
        handlers.AllowedHeaders([]string{"Content-Type"}),
    )

    listener, err := net.Listen("tcp4", "0.0.0.0:8080")
    if err != nil {
        log.Fatal(err)
    }
    log.Println("🔥 SERVER RUNNING ON :8080")
    http.Serve(listener, corsHandler(r))
}

/* -------------------------------------------------------------------------- */
/*                              CREATE ROOM HANDLER                            */
/* -------------------------------------------------------------------------- */

func createRoomHandler(w http.ResponseWriter, r *http.Request) {
    roomID := randomString(6)

    roomsMutex.Lock()
    rooms[roomID] = &Room{clients: make(map[*Client]bool)}
    roomsMutex.Unlock()

    log.Println("🟦 CREATE ROOM:", roomID)
    json.NewEncoder(w).Encode(map[string]string{"roomId": roomID})
}

/* -------------------------------------------------------------------------- */
/*                                VIDEO UPLOAD (A)                             */
/* -------------------------------------------------------------------------- */

func uploadHandler(w http.ResponseWriter, r *http.Request) {
    vars := mux.Vars(r)
    roomID := vars["roomId"]

    log.Println("📹 Uploading video for room:", roomID)

    reader, err := r.MultipartReader()
    if err != nil {
        http.Error(w, "Invalid upload", http.StatusBadRequest)
        return
    }

    var filePart *multipart.Part
    for {
        part, err := reader.NextPart()
        if err == io.EOF {
            break
        }
        if part.FormName() == "file" {
            filePart = part
            break
        }
    }

    if filePart == nil {
        http.Error(w, "Missing file", http.StatusBadRequest)
        return
    }

    os.MkdirAll(uploadDir, 0755)

    ext := filepath.Ext(filePart.FileName())
    if ext == "" {
        ext = ".mp4"
    }

    finalPath := filepath.Join(uploadDir, roomID+ext)
    out, _ := os.Create(finalPath)
    written, _ := io.Copy(out, filePart)
    out.Close()

    log.Printf("📹 SAVED %s (%d bytes)", finalPath, written)

    videoURL := "/uploads/" + roomID + ext

    roomsMutex.Lock()
    room := rooms[roomID]
    roomsMutex.Unlock()

    room.Lock()
    room.videoPath = videoURL
    room.Unlock()

    // Notify peers
    room.Lock()
    for c := range room.clients {
        if c.role == "peer" {
            c.conn.WriteJSON(Message{
                Type:     "video-ready",
                VideoURL: videoURL,
            })
        }
    }
    room.Unlock()

    json.NewEncoder(w).Encode(map[string]string{"videoURL": videoURL})
}

/* -------------------------------------------------------------------------- */
/*                           SUBTITLE UPLOAD (A+B)                             */
/* -------------------------------------------------------------------------- */

func subtitleUploadHandler(w http.ResponseWriter, r *http.Request) {
    vars := mux.Vars(r)
    roomID := vars["roomId"]

    roomsMutex.Lock()
    room, ok := rooms[roomID]
    roomsMutex.Unlock()

    if !ok {
        http.Error(w, "No room", http.StatusNotFound)
        return
    }

    r.ParseMultipartForm(30 << 20)
    file, header, err := r.FormFile("subtitle")
    if err != nil {
        http.Error(w, "Missing subtitle", http.StatusBadRequest)
        return
    }
    defer file.Close()

    raw, _ := io.ReadAll(file)

    ext := filepath.Ext(header.Filename)
    var final []byte
    if ext == ".srt" {
        final = convertSRTtoVTT(string(raw))
    } else {
        final = raw
    }

    os.MkdirAll(uploadDir, 0755)
    subtitlePath := filepath.Join(uploadDir, roomID+".vtt")
    os.WriteFile(subtitlePath, final, 0644)

    subURL := "/uploads/" + roomID + ".vtt"

    room.Lock()
    room.subtitlePath = subURL
    room.Unlock()

    room.Lock()
    for c := range room.clients {
        c.conn.WriteJSON(Message{
            Type:        "control",
            Action:      "subtitle",
            SubtitleURL: subURL,
            Label:       header.Filename,
            Srclang:     "en",
        })
    }
    room.Unlock()

    json.NewEncoder(w).Encode(map[string]string{
        "subtitleURL": subURL,
        "label":       header.Filename,
    })
}

/* -------------------------------------------------------------------------- */
/*                             WEBSOCKET WITH PING (C)                         */
/* -------------------------------------------------------------------------- */

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
    conn, _ := upgrader.Upgrade(w, r, nil)
    client := &Client{conn: conn}

    log.Println("🔌 WS CONNECT")

    // WS KEEPALIVE
    conn.SetReadLimit(512)
    conn.SetReadDeadline(time.Now().Add(60 * time.Second))
    conn.SetPongHandler(func(string) error {
        conn.SetReadDeadline(time.Now().Add(60 * time.Second))
        return nil
    })

    go func() {
        ticker := time.NewTicker(30 * time.Second)
        for range ticker.C {
            conn.WriteControl(websocket.PingMessage, []byte{}, time.Now().Add(5*time.Second))
        }
    }()

    var room *Room
    var roomID string

    defer conn.Close()

    for {
        var msg Message
        err := conn.ReadJSON(&msg)
        if err != nil {
            log.Println("❌ WS Error:", err)
            break
        }

        switch msg.Type {

        case "join":
            roomID = msg.Room
            client.role = msg.Role

            roomsMutex.Lock()
            if rooms[roomID] == nil {
                rooms[roomID] = &Room{clients: make(map[*Client]bool)}
            }
            room = rooms[roomID]
            roomsMutex.Unlock()

            room.Lock()
            room.clients[client] = true

            if client.role == "host" {
                room.host = client
            }

            if client.role == "peer" {
                if room.videoPath != "" {
                    client.conn.WriteJSON(Message{
                        Type:     "video-ready",
                        VideoURL: room.videoPath,
                    })
                }
                if room.subtitlePath != "" {
                    client.conn.WriteJSON(Message{
                        Type:        "control",
                        Action:      "subtitle",
                        SubtitleURL: room.subtitlePath,
                        Label:       "Imported Subtitle",
                        Srclang:     "en",
                    })
                }
                if room.host != nil {
                    room.host.conn.WriteJSON(Message{
                        Type: "peer-joined",
                        Room: roomID,
                    })
                }
            }

            room.Unlock()

        case "sdp", "ice", "control":
            room.Lock()
            for c := range room.clients {
                if c != client {
                    c.conn.WriteJSON(msg)
                }
            }
            room.Unlock()
        }
    }

    /* ---------------------- CLEANUP ON DISCONNECT ---------------------- */

    if room != nil {
        room.Lock()
        delete(room.clients, client)

        if len(room.clients) == 0 {
            if room.videoPath != "" {
                os.Remove(uploadDir + "/" + filepath.Base(room.videoPath))
            }
            if room.subtitlePath != "" {
                os.Remove(uploadDir + "/" + filepath.Base(room.subtitlePath))
            }

            roomsMutex.Lock()
            delete(rooms, roomID)
            roomsMutex.Unlock()
        }

        room.Unlock()
    }
}

/* -------------------------------------------------------------------------- */
/*                                 UTILITIES                                   */
/* -------------------------------------------------------------------------- */

func randomString(n int) string {
    chars := []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789")
    res := make([]rune, n)
    for i := range res {
        res[i] = chars[rand.Intn(len(chars))]
    }
    return string(res)
}

func convertSRTtoVTT(srt string) []byte {
    lines := strings.Split(srt, "\n")
    out := []string{"WEBVTT\n"}
    for _, l := range lines {
        if strings.Contains(l, "-->") {
            l = strings.ReplaceAll(l, ",", ".")
        }
        out = append(out, l)
    }
    return []byte(strings.Join(out, "\n"))
}
