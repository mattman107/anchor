package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"time"
)

type Client struct{
	id uint32
	data map[string]interface{}
	connection *net.Conn
	server *Server
	room *Room
	shutdown chan struct{}
}

func createClient(connection *net.Conn, server *Server) (*Client){
	var id uint32 = rand.Uint32()
	inUse := false
	//determine client id
	for{
		if(server.clients == nil){
			break
		}

		for i := range (*server.clients){
			if((*server.clients)[i].id == id){
				inUse = true
				break
			}
		}
		
		if(!inUse){
			break
		}

		id = rand.Uint32()
	}

	mClient := Client{id: id, connection: connection, server: server, shutdown: make(chan struct{})}
	mClient.log("Connected")

	go mClient.clientHearbeat()	
	go mClient.ClientStuff()

	return &mClient
}

func (client *Client) ClientStuff(){
	dataChannel := make(chan map[string]interface{})
	errorChannel := make(chan error)

	go client.waitForData(dataChannel, errorChannel)

	for{
		select{
		case data := <-dataChannel:
			client.handlePacket(data)
		case err := <- errorChannel:
			if(!errors.Is(err, io.EOF)){
				client.log("Error reading from connection: " + err.Error())
			}
			client.disconnect()
			break
		}
	}
}

func (client *Client) waitForData(dataChannel chan map[string]interface{}, errorChannel chan error){
	buffer := make([]byte, 1024)
	var data []byte

	for{
		select{
		case <- client.shutdown:
			return
		default:
			(*client.connection).SetReadDeadline(time.Now().Add(2 * time.Second))
			count, err := (*client.connection).Read(buffer)
			if(errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, net.ErrClosed)){
				continue
			}

			if(err != nil){
				errorChannel <- err
				return
			}

			newData := buffer[:count]
			data = append(data, newData...)
			
			for{
				delimiterIndex := findDelmiterIndex(data)
				if(delimiterIndex == -1){
					break // Incomplete packet, wait for more data
				}

				packetObject := make(map[string]interface{}, 0)
				if err := json.Unmarshal(data[:delimiterIndex], &packetObject); err != nil {
					fmt.Println("Unable to parse json: " + err.Error())
				} else{
					dataChannel <- packetObject
				}

				data = append(data[:0], data[delimiterIndex + 1:]...)
				//fmt.Println(data)
			}
		}
		
	}
}

func (client *Client) clientHearbeat(){
	for{
		select {
		case <- client.shutdown:
			return
		default: 
			time.Sleep(time.Duration(30) * time.Second)
			go client.sendPacket(map[string]interface{}{
				"type": "Heartbeat",
			})
		}
	}
}

func (client *Client) handlePacket(packetObject map[string]interface{}){
	packetObject["clientId"] = client.id

	packetType, err := packetObject["type"].(string)
	if(!err){
		client.log("Error converting packetType to string")
	}

	packetQuiet, err := packetObject["quiet"].(bool)
	if(!err){
		packetQuiet = false
	}

	if(!packetQuiet && !quietMode){
		client.log("-> " + packetType + " packet")
	}

	//determine what to do with the packet
	if(packetType == "UPDATE_CLIENT_DATA"){
		client.data = packetObject["data"].(map[string]interface{})
	}

	if _, roomId := packetObject["roomId"]; roomId{
		if(client.room == nil){
			room := server.getOrCreateRoom(packetObject["roomId"].(string))
			room.addClient(client)
		}
	}

	if(client.room == nil){
		client.log("Not in a room, ignoring packet")
		return
	}

	if(packetObject["targetClientId"] != nil){
		packetObject["targetClientId"] = uint32(packetObject["targetClientId"].(float64))
		for i:= range (*client.room.clients){
			targetClient := &(*client.room.clients)[i]
			if(targetClient.id == packetObject["targetClientId"]){
				go targetClient.sendPacket(packetObject)
				break	
			}else{
				if(i == len(*client.room.clients) - 1){
					client.log("Target client " + fmt.Sprint(packetObject["targetClientId"]) + " not found")
				}
			}
		}
		return
	}

	switch(packetType){
	case "GAME_COMPLETE":
		client.server.stats.GamesCompleted++
	case "REQUEST_SAVE_STATE":
		if(len(*client.room.clients) > 1){
			(*client.room.requestingStateClients) = append(*client.room.requestingStateClients, *client)
		}
		client.room.broadcastPacket(packetObject, client)
	case "PUSH_SAVE_STATE":
		roomStateRequests := &(*client.room.requestingStateClients)
		for i := range *roomStateRequests{
			mClient := &(*roomStateRequests)[i]
			go mClient.sendPacket(packetObject)
		}
		tmp := make([]Client, 0)
		client.room.requestingStateClients = &tmp
	default:
		client.room.broadcastPacket(packetObject, client)
	}
}

func (client *Client) sendPacket(packetObject map[string]interface{}){
	packetQuiet, bErr := packetObject["quiet"].(bool)
	if(!bErr){
		packetQuiet = false
	}

	if(!packetQuiet && !quietMode){
		packetType := packetObject["type"].(string)
		client.log("<- " + packetType + " packet")
	}

	packet,err := json.Marshal(packetObject)
	if(err != nil){
		client.log("Error creating json packet: " + err.Error())
	}
	packet = append(packet, 0)
	//fmt.Println(packetObject)
	//fmt.Println(packet)

	if client.connection == nil {
		log.Println("connection is nil, cannot write")
		return
	}

	_, wErr := (*client.connection).Write(packet)
	if(wErr != nil){
		client.log("Error sending packet: " + wErr.Error())
		go client.disconnect()
		return
	}
}

func (client *Client) disconnect(){
	go func(){
		client.shutdown <- struct{}{}
	}()

	if(client.room != nil){
		client.room.removeClient(client)
	}
	client.server.removeClient(client)
	
	err := (*client.connection).Close()

	if(err != nil && !errors.Is(err, net.ErrClosed)){
		client.log("Error disconnecting: " + err.Error())
	}

	server.stats.OnlineCount--
	client.log("Disconnected")
}

func (client *Client) log(message string){
	fmt.Println("[" + fmt.Sprint(client.id) + "]:", message)
}