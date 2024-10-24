package main

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
)

// global vars
var port string = "43385"
var quietMode bool = true
var server Server

func main(){
	// loop through args
	for i:=1; i < len(os.Args); i++{
		switch i{
		case 1:
			// port
			portArg, err := strconv.Atoi(os.Args[i])

			if(err != nil){
				fmt.Println("Error setting custom port.")
				break
			}

			// set port to value
			port = strconv.Itoa(portArg)

		case 2:
			// quietMode
			quietArg, err := strconv.ParseBool(os.Args[i])

			if(err != nil){
				fmt.Println("Error setting quiet mode.")
				break
			}

			quietMode = quietArg
		}
	}

	go processStdin()
	server = NewServer()
	server.startServer()
	_ = server.listener

}

func findDelmiterIndex(data []byte) int{
	for i := 0; i < len(data); i++{
		if(data[i] == 0){
			return i
		}
	}

	return -1
}

func getMessage(input []string) string{
	var message bytes.Buffer

	for i := 0; i <= len(input) - 1; i++{
		if(i < len(input)){
			message.WriteString(input[i] + " ")
		}else{
			message.WriteString(input[i])
		}
	}

	return message.String()
}

func sendDisable(client *Client, message string){
	sendServerMessage(client, message)
	client.sendPacket(map[string]interface{}{
		"type": "DISABLE_ANCHOR",
	})
	client.disconnect()
}

func sendServerMessage(client *Client, message string){
	if(message == ""){
		message = "You have been disconnected by the server.\nTry to connect again in a bit!"
	}
	client.sendPacket(map[string]interface{}{
		"type": "SERVER_MESSAGE",
		"message": message,
	})
}

func clientExists(targetIdStr string) *Client{
	targetId, err := strconv.Atoi(targetIdStr)
	if(err != nil){
		return nil
	}
	targetId32 := uint32(targetId)
	
	for i := range *server.clients{
		client := &(*server.clients)[i]

		if(client.id == targetId32){
			return client
		}
	}

	return nil
}

func processStdin(){
	var reader bufio.Reader = *bufio.NewReader(os.Stdin)
	for{
		input, err := reader.ReadString('\n')

		if(err != nil){
			fmt.Println("Error reading from stdin:",err)
			continue
		}

		// remove new line delimiter
		input = strings.Replace(input, "\n", "", 1)

		// split on space
		splitInput := strings.Split(input, " ")

		

		switch splitInput[0]{
		case "roomCount":
			fmt.Println("Room count: ", len(*server.rooms))
		case "clientCount":
			fmt.Println("Client count:", len(*server.clients))
		case "quiet":
			quietMode = !quietMode
			fmt.Println("Quiet mode: ", quietMode)
		case "stats":
			values := reflect.ValueOf(server.stats)
			types := values.Type()
			fmt.Println("Current Stats:")
			for i:= 0 ; i< values.NumField(); i++{
				if(types.Field(i).Name != "ClientSHAs"){
					fmt.Printf("    %s: %v\n",types.Field(i).Name, values.Field(i).Interface())		
				}	
			}

		case "list":
			for _, room := range *server.rooms{
				fmt.Println("Room", room.id + ":")
				for _, client := range *server.clients{
					fmt.Println("  Client", fmt.Sprint(client.id) + ":", client.data)
				}
			}
		case "disable":
			
			targetClientId := splitInput[1]
			client := clientExists(targetClientId)
			
			if(client != nil){
				fmt.Println("[Server] DISABLE_ANCHOR packet ->", client.id)
				go sendDisable(client, getMessage(splitInput[2:]))
				continue
			}

			fmt.Println("Client", targetClientId, "not found")
		case "disableAll":
			fmt.Println("[Server] DISABLE_ANCHOR packet -> All")
			for i := range *server.clients{
				client := &(*server.clients)[i]
				go sendDisable(client, getMessage(splitInput[1:]))
			}
		case "message":
			targetClientId := splitInput[1]
			client := clientExists(targetClientId)
			
			if(client != nil){
				fmt.Println("[Server] SERVER_MESSAGE packet ->", client.id)
				go sendServerMessage(client, getMessage(splitInput[2:]))
				continue
			}

			fmt.Println("Client", targetClientId, "not found")
		case "messageAll":
			fmt.Println("[Server] SERVER_MESSAGE packet -> All")
			for i := range *server.clients{
				client := &(*server.clients)[i]
				go sendServerMessage(client, getMessage(splitInput[1:]))
			}
		case "stop":
			for i := range *server.clients{
				client := &(*server.clients)[i]
				go sendServerMessage(client, "Server restarting. Check back in a bit!")
			}
			server.stats.OnlineCount = 0
			server.saveStats()
			os.Exit(0)
		default:
			fmt.Printf("Available commands:\nhelp: Show this help message\nstats: Print server stats\nquiet: Toggle quiet mode\nroomCount: Show the number of rooms\nclientCount: Show the number of clients\nlist: List all rooms and clients\nstop <message>: Stop the server\nmessage <clientId> <message>: Send a message to a client\nmessageAll <message>: Send a message to all clients\ndisable <clientId> <message>: Disable anchor on a client\ndisableAll <message>: Disable anchor on all clients\n")
		}
	}
}