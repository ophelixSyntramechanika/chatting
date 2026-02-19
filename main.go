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
	port := flag.Int("p", 0, "P2P Port (e.g. 6001)")
	webPort := flag.Int("web", 0, "Web UI Port (e.g. 8081)")
	flag.Parse()

	if *port == 0 || *webPort == 0 {
		fmt.Println("Usage: go run main.go -p <P2P_PORT> -web <WEB_PORT>")
		return
	}

	// INTERNET UPGRADE: We enable NATPortMap and EnableNATService
	// This tells libp2p: "Try to talk to my router and open a door for me."
	var err error
	h, err = libp2p.New(
		libp2p.ListenAddrs(
			multiaddr.StringCast(fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", *port)),
		),
		libp2p.NATPortMap(),      // Automatically try to open ports on your router
		libp2p.EnableNATService(), // Helps peers behind NAT find you
	)
	if err != nil {
		log.Fatal(err)
	}

	h.SetStreamHandler(protocolID, handleStream)

	// Display ALL addresses (including your local network IP)
	fmt.Printf("\n--- P2P CHAT NODE ONLINE ---\n")
	for _, addr := range h.Addrs() {
		fmt.Printf("Address: %s/p2p/%s\n", addr, h.ID().String())
	}
	fmt.Printf("Web UI: http://localhost:%d\n\n", *webPort)

	http.HandleFunc("/", serveHome)
	http.HandleFunc("/ws", handleWebSocket)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%d", *webPort), nil))
}

// --- CORE P2P LOGIC ---

func handleStream(s network.Stream) {
	activePeer = s.Conn().RemotePeer()
	sendToUI("system", "Friend connected!")
	rw := bufio.NewReadWriter(bufio.NewReader(s), bufio.NewWriter(s))
	go readData(rw)
}

func readData(rw *bufio.ReadWriter) {
	for {
		str, err := rw.ReadString('\n')
		if err != nil {
			if err != io.EOF {
				sendToUI("system", "Connection lost.")
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
		sendToUI("error", "Invalid Address format")
		return
	}

	info, err := peer.AddrInfoFromP2pAddr(maddr)
	if err != nil {
		sendToUI("error", "Could not parse Peer info")
		return
	}

	h.Peerstore().AddAddrs(info.ID, info.Addrs, time.Hour)
	
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := h.NewStream(ctx, info.ID, protocolID)
	if err != nil {
		sendToUI("error", "Failed to connect. Peer might be behind a strict firewall.")
		return
	}

	activePeer = info.ID
	sendToUI("system", "Connected!")
	flushQueue(s)
	rw := bufio.NewReadWriter(bufio.NewReader(s), bufio.NewWriter(s))
	go readData(rw)
}

func sendMessage(msg string) {
	msgMutex.Lock()
	defer msgMutex.Unlock()

	if activePeer == "" {
		sendToUI("error", "No active peer!")
		return
	}

	s, err := h.NewStream(context.Background(), activePeer, protocolID)
	if err != nil {
		msgQueue = append(msgQueue, msg)
		sendToUI("error", "Peer offline. Queued.")
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

// --- WEB HELPERS ---

func serveHome(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "index.html")
}

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	wsConn = conn

	// Tell the UI about our main address
	// We pick the first non-local address if possible
	myAddr := fmt.Sprintf("/ip4/127.0.0.1/tcp/%s/p2p/%s", flag.Lookup("p").Value.String(), h.ID())
	sendToUI("my-addr", myAddr)

	for {
		var msg WSMessage
		if err := conn.ReadJSON(&msg); err != nil {
			break
		}
		if msg.Type == "connect" {
			go connectToPeer(msg.Payload)
		} else if msg.Type == "send" {
			go sendMessage(msg.Payload)
		}
	}
}

func sendToUI(msgType, payload string) {
	if wsConn != nil {
		wsConn.WriteJSON(WSMessage{Type: msgType, Payload: payload})
	}
}