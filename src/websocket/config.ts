export enum MeetEvent {
    NOT_FOUND = "not-found",
    BAD_REQUEST = "bad-request",

    JOIN_MEET_LOBBY = 'join-meet-lobby',
    PEER_JOINED_LOBBY = "peer-joined-lobby",

    CREATE_MEET = "create-meet",
    CREATE_MEET_RESPONSE = "create-meet-response",
    JOIN_MEET = "join-meet",
    JOIN_MEET_RESPONSE = "join-meet-response",

    CREATE_OFFER = "create-offer",
    CREATE_OFFER_RESPONSE = "create-offer-response",
    RECEIVED_OFFER = "received-offer",

    CREATE_ANSWER = "create-answer",
    CREATE_ANSWER_RESPONSE = "create-answer-response",
    RECEIVED_ANSWER ="received-answer",
}