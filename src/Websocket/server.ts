import { Socket } from "./socket";
import http from "http";


export function startWebSocketServer(server: http.Server) {
    const socket = new Socket(server);
}
