import { z } from "zod";

export const registerSchema = z.object({
    email: z.string().email(),
    password: z.string().min(8),
    firstName: z.string(),
    lastName: z.string(),
});

export const loginSchema = z.object({
    email: z.string().email(),
    password: z.string().min(8),
});
