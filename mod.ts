import { writeAll } from "https://deno.land/std@0.208.0/streams/write_all.ts";
import { readLines } from "https://deno.land/std@0.208.0/io/read_lines.ts";
import { encodeHex } from "https://deno.land/std@0.208.0/encoding/hex.ts";

const decoder = new TextDecoder();
const encoder = new TextEncoder();

type ClientData = Record<string, any>;

interface BasePacket {
  clientId?: number;
  roomId?: string;
  quiet?: boolean;
  targetClientId?: number;
}

interface UpdateClientDataPacket extends BasePacket {
  type: "UPDATE_CLIENT_DATA";
  data: ClientData;
}

interface AllClientDataPacket extends BasePacket {
  type: "ALL_CLIENT_DATA";
  clients: ClientData[];
}

interface ServerMessagePacket extends BasePacket {
  type: "SERVER_MESSAGE";
  message: string;
}

interface DisableAnchorPacket extends BasePacket {
  type: "DISABLE_ANCHOR";
}

interface OtherPackets extends BasePacket {
  type:
    | "REQUEST_SAVE_STATE"
    | "PUSH_SAVE_STATE"
    | "GAME_COMPLETE"
    | "HEARTBEAT";
}

type Packet =
  | UpdateClientDataPacket
  | DisableAnchorPacket
  | ServerMessagePacket
  | AllClientDataPacket
  | OtherPackets;

interface ServerStats {
  lastStatsHeartbeat: number;
  clientSHAs: Record<string, boolean>;
  onlineCount: number;
  gamesCompleted: number;
  pid: number;
}

const port =
  (Deno.env.has("PORT") && !isNaN(parseInt(Deno.env.get("PORT")!, 10)))
    ? parseInt(Deno.env.get("PORT")!, 10)
    : 43385;
let quietMode = !!Deno.env.has("QUIET");

class Server {
  private listener?: Deno.Listener;
  public clients: Client[] = [];
  public rooms: Room[] = [];
  public stats: ServerStats = {
    lastStatsHeartbeat: Date.now(),
    clientSHAs: {},
    onlineCount: 0,
    gamesCompleted: 0,
    pid: Deno.pid,
  };

  async start() {
    await this.parseStats();

    this.statsHeartbeat();
    this.clientHeartbeat();

    this.startServer();
  }

  async parseStats() {
    try {
      const statsString = await Deno.readTextFile("./stats.json");
      this.stats = Object.assign(this.stats, JSON.parse(statsString));
      this.stats.pid = Deno.pid;
      this.log("Loaded stats file");
    } catch (_) {
      this.log("No stats file found");
    }
  }

  async statsHeartbeat() {
    try {
      this.stats.lastStatsHeartbeat = Date.now();
      this.stats.onlineCount = this.clients.length;

      await this.saveStats();
    } catch (error) {
      this.log(`Error saving stats: ${error.message}`);
    }

    setTimeout(() => {
      this.statsHeartbeat();
    }, 2500);
  }

  async clientHeartbeat() {
    try {
      await Promise.all(server.clients.map((client) => {
        return client.sendPacket({
          type: "HEARTBEAT",
        }).catch((_) => {}); // Ignore errors, client will disconnect if it's a problem
      }));
    } catch (error) {
      this.log(`Error sending heartbeat to clients: ${error.message}`);
    }

    setTimeout(() => {
      this.clientHeartbeat();
    }, 1000 * 30);
  }

  async saveStats() {
    try {
      await Deno.writeTextFile(
        "./stats.json",
        JSON.stringify(this.stats, null, 4),
      );
    } catch (error) {
      this.log(`Error saving stats: ${error.message}`);
    }
  }

  async startServer() {
    this.listener = Deno.listen({ port });

    this.log(`Server Started on port ${port}`);
    try {
      for await (const connection of this.listener) {
        try {
          const client = new Client(connection, this);
          this.clients.push(client);
        } catch (error) {
          this.log(`Error connecting client: ${error.message}`);
        }
      }
    } catch (error) {
      this.log(`Error starting server: ${error.message}`);
    }
  }

  removeClient(client: Client) {
    const index = this.clients.indexOf(client);
    if (index !== -1) {
      this.clients.splice(index, 1);
    }
  }

  getOrCreateRoom(id: string) {
    const room = this.rooms.find((room) => room.id === id);
    if (room) {
      return room;
    }

    const newRoom = new Room(id, this);
    this.rooms.push(newRoom);
    return newRoom;
  }

  removeRoom(room: Room) {
    const index = this.rooms.indexOf(room);
    if (index !== -1) {
      this.rooms.splice(index, 1);
    }
  }

  log(...data: any[]) {
    this.log(`[Server]:`, ...data);
  }
}

class Client {
  public id: number;
  public data: ClientData = {};
  public connection: Deno.Conn;
  public server: Server;
  public room?: Room;

  constructor(connection: Deno.Conn, server: Server) {
    this.connection = connection;
    this.server = server;
    this.id = connection.rid;

    // SHA256 to get a rough idea of how many unique players there are
    crypto.subtle.digest(
      "SHA-256",
      encoder.encode((this.connection.remoteAddr as Deno.NetAddr).hostname),
    )
      .then((hasBuffer) => {
        this.server.stats.onlineCount++;
        this.server.stats.clientSHAs[encodeHex(hasBuffer)] = true;
      })
      .catch((error) => {
        this.log(`Error hashing client: ${error.message}`);
      });

    this.waitForData();
    this.log("Connected");
  }

  async waitForData() {
    const buffer = new Uint8Array(1024);
    let data = new Uint8Array(0);

    while (true) {
      let count: null | number = 0;

      try {
        count = await this.connection.read(buffer);
      } catch (error) {
        this.log(`Error reading from connection: ${error.message}`);
        this.disconnect();
        break;
      }

      if (!count) {
        this.disconnect();
        break;
      }

      // Concatenate received data with the existing data
      const receivedData = buffer.subarray(0, count);
      data = concatUint8Arrays(data, receivedData);

      // Handle all complete packets (while loop in case multiple packets were received at once)
      while (true) {
        const delimiterIndex = findDelimiterIndex(data);
        if (delimiterIndex === -1) {
          break; // Incomplete packet, wait for more data
        }

        // Extract the packet
        const packet = data.subarray(0, delimiterIndex);
        data = data.subarray(delimiterIndex + 1);

        this.handlePacket(packet);
      }
    }
  }

  handlePacket(packet: Uint8Array) {
    try {
      const packetString = decoder.decode(packet);
      const packetObject: Packet = JSON.parse(packetString);
      packetObject.clientId = this.id;

      if (!packetObject.quiet && !quietMode) {
        this.log(`-> ${packetObject.type} packet`);
      }

      if (packetObject.type === "UPDATE_CLIENT_DATA") {
        this.data = packetObject.data;
      }

      if (packetObject.type === "GAME_COMPLETE") {
        this.server.stats.gamesCompleted++;
      }

      if (packetObject.roomId && !this.room) {
        this.server.getOrCreateRoom(packetObject.roomId).addClient(this);
      }

      if (!this.room) {
        this.log("Not in a room, ignoring packet");
        return;
      }

      if (packetObject.targetClientId) {
        const targetClient = this.room.clients.find((client) =>
          client.id === packetObject.targetClientId
        );
        if (targetClient) {
          targetClient.sendPacket(packetObject);
        } else {
          this.log(`Target client ${packetObject.targetClientId} not found`);
        }
        return;
      }

      if (packetObject.type === "REQUEST_SAVE_STATE") {
        if (this.room.clients.length > 1) {
          this.room.requestingStateClients.push(this);
          this.room.broadcastPacket(packetObject, this);
        }
      } else if (packetObject.type === "PUSH_SAVE_STATE") {
        const roomStateRequests = this.room.requestingStateClients;
        roomStateRequests.forEach((client) => {
          client.sendPacket(packetObject);
        });
        this.room.requestingStateClients = [];
      } else {
        this.room.broadcastPacket(packetObject, this);
      }
    } catch (error) {
      this.log(`Error handling packet: ${error.message}`);
    }
  }

  async sendPacket(packetObject: Packet) {
    try {
      if (!packetObject.quiet && !quietMode) {
        this.log(`<- ${packetObject.type} packet`);
      }
      const packetString = JSON.stringify(packetObject);
      const packet = encoder.encode(packetString + "\0");

      // Wait for writeAll to complete, if it takes longer than 30 seconds, disconnect
      await Promise.race([
        writeAll(this.connection, packet),
        new Promise((_, reject) => {
          setTimeout(() => {
            reject(new Error("Timeout, took longer than 30 seconds to send"));
          }, 1000 * 30);
        }),
      ]);
    } catch (error) {
      this.log(`Error sending packet: ${error.message}`);
      this.disconnect();
    }
  }

  disconnect() {
    try {
      if (this.room) {
        this.room.removeClient(this);
      }
      this.server.removeClient(this);
      this.connection.close();
    } catch (error) {
      this.log(`Error disconnecting: ${error.message}`);
    } finally {
      this.server.stats.onlineCount--;
      this.log("Disconnected");
    }
  }

  log(message: string) {
    this.log(`[Client ${this.id}]: ${message}`);
  }
}

class Room {
  public id: string;
  public server: Server;
  public clients: Client[] = [];
  public requestingStateClients: Client[] = [];

  constructor(id: string, server: Server) {
    this.id = id;
    this.server = server;
    this.log("Created");
  }

  addClient(client: Client) {
    this.log(`Adding client ${client.id}`);
    this.clients.push(client);
    client.room = this;

    this.broadcastAllClientData();
  }

  removeClient(client: Client) {
    this.log(`Removing client ${client.id}`);
    const index = this.clients.indexOf(client);
    if (index !== -1) {
      this.clients.splice(index, 1);
      client.room = undefined;
    }

    if (this.clients.length) {
      this.broadcastAllClientData();
    } else {
      this.log("No clients left, removing room");
      this.server.removeRoom(this);
    }
  }

  broadcastAllClientData() {
    if (!quietMode) {
      this.log("<- ALL_CLIENT_DATA packet");
    }
    for (const client of this.clients) {
      const packetObject = {
        type: "ALL_CLIENT_DATA" as const,
        roomId: this.id,
        clients: this.clients.filter((c) => c !== client).map((c) => ({
          clientId: c.id,
          ...c.data,
        })),
      };

      client.sendPacket(packetObject);
    }
  }

  broadcastPacket(packetObject: Packet, sender: Client) {
    if (!packetObject.quiet && !quietMode) {
      this.log(`<- ${packetObject.type} packet from ${sender.id}`);
    }

    for (const client of this.clients) {
      if (client !== sender) {
        client.sendPacket(packetObject);
      }
    }
  }

  log(message: string) {
    this.log(`[Room ${this.id}]: ${message}`);
  }
}

function concatUint8Arrays(a: Uint8Array, b: Uint8Array): Uint8Array {
  const result = new Uint8Array(a.length + b.length);
  result.set(a, 0);
  result.set(b, a.length);
  return result;
}

function findDelimiterIndex(data: Uint8Array): number {
  for (let i = 0; i < data.length; i++) {
    if (data[i] === 0 /* null terminator */) {
      return i;
    }
  }
  return -1;
}

const server = new Server();
server.start().catch((error) => {
  console.error("Error starting server: ", error);
  Deno.exit(1);
});

globalThis.addEventListener("unhandledrejection", (e) => {
  console.error("Unhandled rejection at:", e.promise, "reason:", e.reason);
  e.preventDefault();
  Deno.exit(1);
});

function sendServerMessage(client: Client, message: string) {
  return client.sendPacket({
    type: "SERVER_MESSAGE",
    message,
  });
}

function sendDisable(client: Client, message: string) {
  sendServerMessage(client, message)
    .finally(() =>
      client.sendPacket({
        type: "DISABLE_ANCHOR",
      })
    );
}

async function stop(message = "Server restarting") {
  await Promise.all(
    server.clients.map((client) =>
      sendServerMessage(client, message)
        .finally(() => {
          client.disconnect();
        })
    ),
  );

  Deno.exit();
}

(async function processStdin() {
  try {
    for await (const line of readLines(Deno.stdin)) {
      const [command, ...args] = line.split(" ");

      switch (command) {
        default:
        case "help": {
          this.log(
            `Available commands:
  help: Show this help message
  stats: Print server stats
  quiet: Toggle quiet mode
  roomCount: Show the number of rooms
  clientCount: Show the number of clients
  list: List all rooms and clients
  stop <message>: Stop the server
  message <clientId> <message>: Send a message to a client
  messageAll <message>: Send a message to all clients
  disable <clientId> <message>: Disable anchor on a client
  disableAll <message>: Disable anchor on all clients`,
          );
          break;
        }
        case "roomCount": {
          this.log(`Room count: ${server.rooms.length}`);
          break;
        }
        case "clientCount": {
          this.log(`Client count: ${server.clients.length}`);
          break;
        }
        case "quiet": {
          quietMode = !quietMode;
          this.log(`Quiet mode: ${quietMode}`);
          break;
        }
        case "stats": {
          const { clientSHAs: _, ...stats } = server.stats;
          this.log(stats);
          break;
        }
        case "list": {
          for (const room of server.rooms) {
            this.log(`Room ${room.id}:`);
            for (const client of room.clients) {
              this.log(
                `  Client ${client.id}: ${JSON.stringify(client.data)}`,
              );
            }
          }
          break;
        }
        case "disable": {
          const [clientId, ...messageParts] = args;
          const message = messageParts.join(" ");
          const client = server.clients.find((c) =>
            c.id === parseInt(clientId, 10)
          );
          if (client) {
            sendDisable(client, message);
          } else {
            this.log(`Client ${clientId} not found`);
          }
          break;
        }
        case "disableAll": {
          const message = args.join(" ");
          for (const client of server.clients) {
            sendDisable(client, message);
          }
          break;
        }
        case "message": {
          const [clientId, ...messageParts] = args;
          const message = messageParts.join(" ");
          const client = server.clients.find((c) =>
            c.id === parseInt(clientId, 10)
          );
          if (client) {
            sendServerMessage(client, message);
          } else {
            this.log(`Client ${clientId} not found`);
          }
          break;
        }
        case "messageAll": {
          const message = args.join(" ");
          for (const client of server.clients) {
            sendServerMessage(client, message);
          }
          break;
        }
        case "stop": {
          const message = args.join(" ");
          stop(message);
          break;
        }
      }
    }
  } catch (error) {
    console.error("Error reading from stdin: ", error.message);
    processStdin();
  }
})();
