import { z, ZodEffects } from "zod";
import { MeetEvent } from "../config";

export interface ParsedMessageData {
    token?: string;
    type: MeetEvent;
    meetCode: string;
    sessionId: string;
    data: {
        offer?: string;
        answer?: string;
    },
}

const withTokenOrName = <T extends z.ZodTypeAny>(schema: T) =>
    schema.refine((data) => data.token || data.name, {
        message: "Either token or name must be provided",
        path: ["token", "name"],
    });

const BaseSchema = z.object({
    token: z.string().optional(),
    name: z.string().optional(),
    meetCode: z.string(),
});

// For message that can be initial, so without a session id
const JoiningMsgSchema = BaseSchema.extend({
    sessionId: z.string().optional(),
});

const SubsequentMsgSchema = BaseSchema.extend({
    sessionId: z.string(),
});

export const MessageSchemas: Record<any, ZodEffects<any>> = {
    [MeetEvent.JOIN_MEET_LOBBY]: withTokenOrName(
        JoiningMsgSchema.extend({
            type: z.enum([MeetEvent.JOIN_MEET_LOBBY]),
        }),
    ),
    [MeetEvent.LEAVE_MEET]: withTokenOrName(
        SubsequentMsgSchema.extend({
            type: z.enum([MeetEvent.LEAVE_MEET]),
        }),
    ),
};
