export type ClientData = Record<string, unknown>;

export interface BasePacket {
  clientId?: number;
  roomId?: string;
  quiet?: boolean;
  targetClientId?: number;
  comp?: boolean;
}

export interface UpdateClientDataPacket extends BasePacket {
  type: "UPDATE_CLIENT_DATA";
  handlesCompression: boolean;
  data: ClientData;
}

export interface AllClientDataPacket extends BasePacket {
  type: "ALL_CLIENT_DATA";
  clients: ClientData[];
}

export interface ServerMessagePacket extends BasePacket {
  type: "SERVER_MESSAGE";
  message: string;
}

export interface DisableAnchorPacket extends BasePacket {
  type: "DISABLE_ANCHOR";
}

export interface OtherPackets extends BasePacket {
  type:
    | "REQUEST_SAVE_STATE"
    | "PUSH_SAVE_STATE"
    | "GAME_COMPLETE"
    | "HEARTBEAT";
}

export type Packet =
  | UpdateClientDataPacket
  | DisableAnchorPacket
  | ServerMessagePacket
  | AllClientDataPacket
  | OtherPackets;
