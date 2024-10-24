package main

import (
	"fmt"
	"slices"
)

type Room struct{
	id string
	server *Server
	clients *[]Client
	requestingStateClients *[]Client
}

func newRoom(id string, server *Server) (Room){
	clients := make([]Client,0)
	requetingClients := make([]Client,0)
	room := Room{id: id, server: server, clients: &clients, requestingStateClients: &requetingClients}
	room.log("Created")
	return room
}

func (room *Room) broadcastAllClientData(){
	if(!quietMode){
		room.log("<- ALL_CLIENT_DATA packet")
	}

	for i := range (*room.clients){
		client := &(*room.clients)[i]
		clientsDataObject := make([]map[string]interface{}, 0)
		for _, mClient := range (*room.clients){
			if(mClient.id != client.id){
				clientDataObject := make(map[string]interface{}, 0)
				clientDataObject["clientId"] = mClient.id
				for key, value := range mClient.data{
					clientDataObject[key] = value
				}
				clientsDataObject = append(clientsDataObject, clientDataObject)
			}
		}

		packetObject := map[string]interface{}{
			"type": "ALL_CLIENT_DATA",
			"roomId": room.id,
			"clients": clientsDataObject,
		}
		fmt.Println(client.id)
		fmt.Println(client.data)
		fmt.Println(clientsDataObject)
		fmt.Println()
		go client.sendPacket(packetObject)
	}
}

func (room *Room) broadcastPacket(packetObject map[string]interface{}, sender *Client){
	packetQuiet, err := packetObject["quiet"].(bool)
	if(!err){
		packetQuiet = true
	}

	if(!packetQuiet && !quietMode){
		room.log("<- " + packetObject["type"].(string) + " packet from " + fmt.Sprint(sender.id))
	}

	for i := range *room.clients{
		client := &(*room.clients)[i]
		if(client.id != sender.id){
			go client.sendPacket(packetObject)
		}
	}
}

func (room *Room) addClient(client *Client){
	room.log("Adding client " + fmt.Sprint(client.id))
	*room.clients = append((*room.clients), *client)

	client.room = room
	go room.broadcastAllClientData()
}

func (room *Room) removeClient(client *Client){
	room.log("Removing client " + fmt.Sprint(client.id))
	
	index := slices.IndexFunc((*room.clients), func(c Client) bool { return c.id == client.id })
	if(index != -1){
		(*room.clients) = append((*room.clients)[:index], (*room.clients)[index + 1:]...)
		client.room = nil
	}

	if(len(*room.clients) > 0){
		room.broadcastAllClientData()
	}else{
		room.log("No clients left, removing room")
		room.server.removeRoom(room)
	}
}

func (room *Room) log(message string){
	fmt.Println("[Room " + room.id +"]:", message)
}