package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"slices"
	"strings"
	"time"
)
type ServerStats struct{
	LastStatsHeartbeat time.Time `json:"lastStatsHeartbeat"`
	ClientSHAs []string `json:"clientSHAs"`
	OnlineCount int `json:"onlineCount"`
	GamesCompleted int `json:"gamesCompleted"`
	Pid int `json:"pid"`
}

type Server struct{
	listener net.Listener
	clients *[]Client
	rooms *[]Room
	stats ServerStats
}

func NewServer() (Server){
	// create the listener
	ln, err := net.Listen("tcp", "localhost:" + port)

	if err != nil {
		// handle error
		fmt.Println("Error Starting Server on port "  + port)
		os.Exit(-1)
	}

	fmt.Println("Server Started on port", port)
	fmt.Println("Quiet mode:", quietMode)

	clients := make([]Client, 0)
	rooms := make([]Room, 0)

	return Server{listener: ln, clients: &clients, rooms: &rooms}
}

func (server *Server) startServer(){
	// load stats
	server.parseStats()
	server.stats.Pid = os.Getpid()
	// heart beat
	go server.statsHeartbeat()

	// loop waiting for connection
	for {
		conn, err := server.listener.Accept()
		if err != nil {
			// handle error
			fmt.Println("Connection Failed")
			continue
		}

		//create client
		var newClient *Client = createClient(&conn, server)
		server.stats.OnlineCount++
		//SHA256
		hash := sha256.Sum256([]byte(strings.Split((*newClient.connection).RemoteAddr().String(), ":")[0]))
		conv := hex.EncodeToString(hash[:])
		if(!slices.Contains(server.stats.ClientSHAs, conv)){
			server.stats.ClientSHAs = append(server.stats.ClientSHAs, conv)
		}
		//push to client slice
		*server.clients = append(*server.clients, *newClient)
	}
}

func (server *Server) parseStats(){
	file, err := os.ReadFile("stats.json")
	if(err != nil){
		fmt.Println("Error reading stats.json file:", err)
	}
	err = json.Unmarshal(file, &server.stats)
	if(err != nil){
		fmt.Println("Error parsing stats.json file:", err)
	}
}

func (server *Server) saveStats(){
	bytes, err:=json.MarshalIndent(server.stats, "", "    ")
	if(err != nil){
		fmt.Println("Error converting stats object to json:", err)
	}
	err = os.WriteFile("./stats.json", bytes, 0644)
	if(err != nil){
		fmt.Println("Error writing json to file:", err)
	}
}

func (server *Server) statsHeartbeat(){
	for{
		server.stats.LastStatsHeartbeat = time.Now().Local()
		server.stats.OnlineCount = len(*server.clients)

		server.saveStats()

		time.Sleep(time.Duration(30) * time.Second)
	}
}

func (server *Server) removeClient(client *Client){
	index := slices.IndexFunc(*server.clients, func(c Client) bool { return c.id == client.id })
	if(index != -1){
		*server.clients = append((*server.clients)[:index], (*server.clients)[index + 1:]...)
	}
}

func (server *Server) removeRoom(room *Room){
	index := slices.IndexFunc(*server.rooms, func(r Room) bool { return r.id == room.id })
	if(index != -1){
		*server.rooms = append((*server.rooms)[:index], (*server.rooms)[index + 1:]...)
	}
}

func (server *Server) getOrCreateRoom(roomId string) (*Room){
	for i := range *server.rooms{
		if(roomId == (*server.rooms)[i].id){
			return &(*server.rooms)[i]
		}
	}
	
	newRoom := newRoom(roomId, server)
	*server.rooms = append((*server.rooms), newRoom)
	return  &newRoom
}