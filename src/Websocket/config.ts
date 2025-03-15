export enum GeneralMessage {
    BAD_REQUEST = "bad-request",
    MEET_NOT_FOUND = "not-found",
}

export enum MeetEvent {
    JOIN_MEET_LOBBY = "join-meet-lobby", // Client created event
    MEET_LOBBY_UPDATED = "meet-lobby-updated", // Server generated event

    CREATE_OFFER = "create-offer", // Client created event
    OFFER = "offer", // Server generated event

    CREATE_ANSWER = "create-answer", // Client created event
    ANSWER = "answer", // Server generated event

    PEER_JOINED = "peer-joined", // Server generated event

    LEAVE_MEET = "leave-meet", // Client created event
    PEER_LEFT = "peer-left", // Server generated event
}