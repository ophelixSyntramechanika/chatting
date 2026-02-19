package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"
)

// --- GLOBAL VARIABLES ---
var (
	h            host.Host
	wsConn       *websocket.Conn
	wsMutex      sync.Mutex // protects wsConn writes
	msgMutex     sync.Mutex
	activePeer   peer.ID
	activeStream network.Stream
	msgQueue     []string
	p2pPort      int
)

const protocolID = "/chat/1.0.0"

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type WSMessage struct {
	Type    string `json:"type"`
	Payload string `json:"payload"`
}

func main() {
	port := flag.Int("p", 0, "P2P Port")
	webPort := flag.Int("web", 0, "Web UI Port")
	flag.Parse()

	if *port == 0 || *webPort == 0 {
		fmt.Println("Usage: go run main.go -p <P2P_PORT> -web <WEB_PORT>")
		fmt.Println("Example: go run main.go -p 6001 -web 8081")
		return
	}

	p2pPort = *port

	var err error
	h, err = libp2p.New(
		libp2p.ListenAddrs(
			multiaddr.StringCast(fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", *port)),
		),
		libp2p.NATPortMap(),
		libp2p.EnableNATService(),
		libp2p.EnableRelay(),
		libp2p.EnableHolePunching(),
	)
	if err != nil {
		log.Fatal("Failed to create libp2p host:", err)
	}

	h.SetStreamHandler(protocolID, handleIncomingStream)

	fmt.Printf("\n========================================\n")
	fmt.Printf("  P2P CHAT NODE ONLINE\n")
	fmt.Printf("========================================\n")
	fmt.Printf("PEER ID: %s\n\n", h.ID().String())
	fmt.Println("YOUR ADDRESSES (copy the full line to your friend):")
	for _, addr := range h.Addrs() {
		full := fmt.Sprintf("%s/p2p/%s", addr, h.ID().String())
		fmt.Printf("  %s\n", full)
	}
	fmt.Printf("\nLocal Web UI: http://localhost:%d\n", *webPort)
	fmt.Printf("========================================\n\n")

	http.HandleFunc("/", serveHome)
	http.HandleFunc("/ws", handleWebSocket)

	log.Printf("Web server starting on port %d...\n", *webPort)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *webPort), nil))
}

// --- INCOMING CONNECTION HANDLER ---

func handleIncomingStream(s network.Stream) {
	msgMutex.Lock()
	activePeer = s.Conn().RemotePeer()
	activeStream = s
	msgMutex.Unlock()

	sendToUI("system", "✅ Friend connected! You can now chat.")
	listenOnStream(s)
}

func listenOnStream(s network.Stream) {
	reader := bufio.NewReader(s)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				log.Println("Stream read error:", err)
			}
			sendToUI("system", "⚠️ Friend disconnected.")
			msgMutex.Lock()
			activePeer = ""
			activeStream = nil
			msgMutex.Unlock()
			return
		}
		line = strings.TrimSpace(line)
		if line != "" {
			sendToUI("msg", "Friend: "+line)
		}
	}
}

// --- OUTGOING CONNECTION ---

func connectToPeer(targetStr string) {
	// Clean the input - this is the #1 cause of "format error"
	targetStr = strings.TrimSpace(targetStr)

	fmt.Printf("\n[DEBUG] Attempting to connect to: [%s]\n", targetStr)

	if targetStr == "" {
		sendToUI("error", "❌ Address is empty! Paste the full /ip4/... address.")
		return
	}

	if !strings.HasPrefix(targetStr, "/") {
		sendToUI("error", "❌ Address must start with /ip4/ or /ip6/ — you pasted: "+targetStr)
		return
	}

	maddr, err := multiaddr.NewMultiaddr(targetStr)
	if err != nil {
		fmt.Printf("[DEBUG] Multiaddr parse error: %v\n", err)
		sendToUI("error", "❌ Bad address format. Error: "+err.Error())
		return
	}

	info, err := peer.AddrInfoFromP2pAddr(maddr)
	if err != nil {
		fmt.Printf("[DEBUG] AddrInfo error: %v\n", err)
		sendToUI("error", "❌ Cannot extract peer info. Make sure address ends with /p2p/PeerID")
		return
	}

	sendToUI("system", "🔄 Connecting (this may take up to 15 seconds)...")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Step 1: Connect to the peer
	if err := h.Connect(ctx, *info); err != nil {
		fmt.Printf("[DEBUG] Connect error: %v\n", err)
		sendToUI("error", "❌ Could not reach peer: "+err.Error())
		return
	}

	// Step 2: Open a chat stream
	s, err := h.NewStream(ctx, info.ID, protocolID)
	if err != nil {
		fmt.Printf("[DEBUG] NewStream error: %v\n", err)
		sendToUI("error", "❌ Connected but stream failed: "+err.Error())
		return
	}

	msgMutex.Lock()
	activePeer = info.ID
	activeStream = s
	// Flush any queued messages
	for _, qMsg := range msgQueue {
		s.Write([]byte(qMsg + "\n"))
	}
	msgQueue = []string{}
	msgMutex.Unlock()

	sendToUI("system", "✅ Connected! You can now chat.")
	go listenOnStream(s)
}

// --- SEND MESSAGE ---

func sendMessage(msg string) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return
	}

	msgMutex.Lock()
	defer msgMutex.Unlock()

	if activePeer == "" {
		sendToUI("error", "❌ Not connected to anyone! Connect first.")
		return
	}

	if activeStream == nil {
		// Try to open a new stream to the same peer
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s, err := h.NewStream(ctx, activePeer, protocolID)
		if err != nil {
			msgQueue = append(msgQueue, msg)
			sendToUI("error", "⚠️ Peer offline. Message queued.")
			return
		}
		activeStream = s
		go listenOnStream(s)
	}

	_, err := activeStream.Write([]byte(msg + "\n"))
	if err != nil {
		msgQueue = append(msgQueue, msg)
		activeStream = nil
		sendToUI("error", "⚠️ Send failed. Message queued.")
		return
	}

	sendToUI("me", "You: "+msg)
}

// --- WEB SERVER ---

func serveHome(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "index.html")
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket upgrade error:", err)
		return
	}

	wsMutex.Lock()
	wsConn = conn
	wsMutex.Unlock()

	// Send my address to the UI immediately
	myAddr := fmt.Sprintf("/ip4/127.0.0.1/tcp/%d/p2p/%s", p2pPort, h.ID().String())
	sendToUI("my-addr", myAddr)
	sendToUI("system", "✅ Backend connected! Copy your address above and share it.")

	for {
		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			log.Println("WebSocket read error:", err)
			break
		}

		switch msg.Type {
		case "connect":
			go connectToPeer(msg.Payload)
		case "send":
			go sendMessage(msg.Payload)
		}
	}

	wsMutex.Lock()
	wsConn = nil
	wsMutex.Unlock()
}

func sendToUI(msgType, payload string) {
	wsMutex.Lock()
	defer wsMutex.Unlock()
	if wsConn != nil {
		err := wsConn.WriteJSON(WSMessage{Type: msgType, Payload: payload})
		if err != nil {
			log.Println("sendToUI error:", err)
		}
	}
}
