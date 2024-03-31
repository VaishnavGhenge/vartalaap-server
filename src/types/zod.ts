import { z } from "zod";

export const baseRequestBodySchema = z.object({
    sessionId: z.string(),
    meetId: z.string(),
});

export const joinMeetQueryParamsSchema = z.object({
    meetId: z.string(),
});

export const UserSignupRequestBodySchema = z.object({
    email: z.string().email(),
    password: z.string(),
    profile_picture: z.string().optional(),
    firstname: z.string(),
    surname: z.string(),
});

export const UserLoginRequestBodySchema = z.object({
    email: z.string().email(),
    password: z.string(),
});