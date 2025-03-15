import { WebSocket } from "ws";

import { MeetEvent } from "./config";
import { logger } from "../Logger/logger";
import { sendToMeetPeers } from "./utils";
import { ParsedMessageData } from "./validation/Message";
import { AuthenticatedSession, Session, UnAuthenticatedSession } from "./socket";

export class Meet {
    private clients = new Map<string, { meetCode: string; ws: WebSocket }>();
    private meetPeers = new Map<string, { inLobby: Set<string>; inMeet: Set<string> }>();


    public process(session: Session | UnAuthenticatedSession | AuthenticatedSession, ws: WebSocket, message: ParsedMessageData) {
        const { type, meetCode, data } = message;
        this.clients.set(session.id, { meetCode, ws });

        const handlers: Record<string, Function> = {
            [MeetEvent.JOIN_MEET_LOBBY]: () => this.joinLobby(meetCode, session.id),
            [MeetEvent.CREATE_OFFER]: () => {
                this.join(meetCode, session.id);
                this.relayMessage(meetCode, session.id, MeetEvent.OFFER, data.offer);
            },
            [MeetEvent.CREATE_ANSWER]: () => this.relayMessage(meetCode, session.id, MeetEvent.ANSWER, data.answer),
            [MeetEvent.LEAVE_MEET]: () => this.leave(meetCode, session.id),
        };

        if (handlers[type]) {
            handlers[type]();
        } else {
            logger.error(`Unknown message type: ${type}`);
        }
    }

    private getOrCreateMeet(meetCode: string) {
        if (!this.meetPeers.has(meetCode)) {
            this.meetPeers.set(meetCode, { inLobby: new Set(), inMeet: new Set() });
        }
        return this.meetPeers.get(meetCode)!;
    }

    private joinLobby(meetCode: string, sessionId: string) {
        this.getOrCreateMeet(meetCode).inLobby.add(sessionId);
        logger.info(`User ${sessionId} joined lobby for meet ${meetCode}`);
    }

    private join(meetCode: string, sessionId: string) {
        const meet = this.getOrCreateMeet(meetCode);
        meet.inLobby.delete(sessionId);
        meet.inMeet.add(sessionId);
        this.clients.set(sessionId, { meetCode, ws: this.clients.get(sessionId)!.ws });
        logger.info(`User ${sessionId} joined meet ${meetCode}`);
    }

    private relayMessage(meetCode: string, sessionId: string, type: string, payload: any) {
        sendToMeetPeers(meetCode, { type, sessionId, payload }, this.clients.get(sessionId)!.ws, false);
    }

    private leave(meetCode: string, sessionId: string) {
        const meet = this.meetPeers.get(meetCode);
        if (!meet) return;

        const { ws } = this.clients.get(sessionId)!;

        meet.inMeet.delete(sessionId);
        meet.inLobby.delete(sessionId);
        this.clients.delete(sessionId);

        sendToMeetPeers(meetCode, { type: MeetEvent.PEER_LEFT, sessionId }, ws, true);
        logger.info(`User ${sessionId} left meet ${meetCode}`);
    }

    public handleDisconnection(session: Session) {
        const meetCode = this.clients.get(session.id)?.meetCode;
        if (meetCode) this.leave(meetCode, session.id);
    }
}
