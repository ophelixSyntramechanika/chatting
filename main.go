package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
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
	h          host.Host
	wsConn     *websocket.Conn
	msgMutex   sync.Mutex
	activePeer peer.ID
	msgQueue   []string
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
		return
	}

	// PRO P2P CONFIGURATION
	var err error
	h, err = libp2p.New(
		libp2p.ListenAddrs(
			multiaddr.StringCast(fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", *port)),
		),
		libp2p.NATPortMap(),         // Try to open router port
		libp2p.EnableNATService(),   // Help others find you
		libp2p.EnableRelay(),        // Enable Circuit Relay v2
		libp2p.EnableHolePunching(), // Active firewall bypassing
	)
	if err != nil {
		log.Fatal(err)
	}

	// Set the listener for incoming messages
	h.SetStreamHandler(protocolID, handleStream)

	// Print Addresses to Terminal
	fmt.Printf("\n--- P2P CHAT NODE ONLINE ---\n")
	fmt.Printf("PEER ID: %s\n", h.ID().String())
	fmt.Println("\nCOPY ONE OF THESE ADDRESSES TO YOUR FRIEND:")
	for _, addr := range h.Addrs() {
		fmt.Printf("Address: %s/p2p/%s\n", addr, h.ID().String())
	}
	fmt.Printf("\nWeb UI: http://localhost:%d\n", *webPort)
	fmt.Printf("Vercel UI: Your Vercel Link (Make sure to allow Insecure Content)\n\n")

	// Start Web Server
	http.HandleFunc("/", serveHome)
	http.HandleFunc("/ws", handleWebSocket)

	go func() {
		fmt.Printf("Starting web server on port %d...\n", *webPort)
		err := http.ListenAndServe(fmt.Sprintf(":%d", *webPort), nil)
		if err != nil {
			log.Printf("Web server error: %v\n", err)
			log.Fatal(err)
		}
	}()

	fmt.Println("P2P Chat Node is running. Press Ctrl+C to stop.")
	select {} // Keep the program running forever
}

// --- P2P STREAM LOGIC ---

func handleStream(s network.Stream) {
	activePeer = s.Conn().RemotePeer()
	sendToUI("system", "Friend connected directly!")
	rw := bufio.NewReadWriter(bufio.NewReader(s), bufio.NewWriter(s))
	go readData(rw)
}

func readData(rw *bufio.ReadWriter) {
	for {
		str, err := rw.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				log.Println("Read error:", err)
			}
			return
		}
		if str != "" {
			sendToUI("msg", str)
		}
	}
}

func connectToPeer(targetStr string) {
	maddr, err := multiaddr.NewMultiaddr(targetStr)
	if err != nil {
		sendToUI("error", "Format error: Use /ip4/IP/tcp/PORT/p2p/ID")
		return
	}

	info, err := peer.AddrInfoFromP2pAddr(maddr)
	if err != nil {
		sendToUI("error", "Invalid Peer Info")
		return
	}

	// Add to address book
	h.Peerstore().AddAddrs(info.ID, info.Addrs, time.Hour)

	// Try connecting with a 15-second timeout (Hole punching takes time)
	sendToUI("system", "Attempting connection (Hole Punching)...")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	s, err := h.NewStream(ctx, info.ID, protocolID)
	if err != nil {
		sendToUI("error", "Connection failed. Checking Relay...")
		return
	}

	activePeer = info.ID
	sendToUI("system", "Connected successfully!")
	flushQueue(s)
	rw := bufio.NewReadWriter(bufio.NewReader(s), bufio.NewWriter(s))
	go readData(rw)
}

func sendMessage(msg string) {
	msgMutex.Lock()
	defer msgMutex.Unlock()

	if activePeer == "" {
		sendToUI("error", "Not connected to anyone!")
		return
	}

	s, err := h.NewStream(context.Background(), activePeer, protocolID)
	if err != nil {
		msgQueue = append(msgQueue, msg)
		sendToUI("error", "Peer offline. Message queued.")
		return
	}

	_, err = s.Write([]byte(msg + "\n"))
	if err != nil {
		msgQueue = append(msgQueue, msg)
	}
}

func flushQueue(s network.Stream) {
	msgMutex.Lock()
	defer msgMutex.Unlock()
	for _, msg := range msgQueue {
		s.Write([]byte(msg + "\n"))
	}
	msgQueue = []string{}
}

// --- COMMUNICATION BRIDGE ---

func serveHome(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "index.html")
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WS Upgrade Error:", err)
		return
	}
	wsConn = conn

	// Auto-send the most likely local address to the UI
	myAddr := fmt.Sprintf("/ip4/127.0.0.1/tcp/%s/p2p/%s", flag.Lookup("p").Value.String(), h.ID())
	sendToUI("my-addr", myAddr)

	for {
		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			break
		}
		switch msg.Type {
		case "connect":
			go connectToPeer(msg.Payload)
		case "send":
			go sendMessage(msg.Payload)
		}
	}
}

func sendToUI(msgType, payload string) {
	if wsConn != nil {
		wsConn.WriteJSON(WSMessage{Type: msgType, Payload: payload})
	}
}
