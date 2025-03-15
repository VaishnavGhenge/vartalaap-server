import { z } from "zod";

export const baseRequestBodySchema = z.object({
    sessionId: z.string(),
    meetId: z.string(),
});

export const joinMeetQueryParamsSchema = z.object({
    meetId: z.string(),
});
