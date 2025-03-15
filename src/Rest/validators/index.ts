import { Request, Response, NextFunction } from "express";
import { z, ZodType } from "zod";

export const validate = <T extends { [key: string]: z.ZodType }>(
    schema: z.ZodObject<T> | z.ZodEffects<z.ZodObject<T>>,
    requestAttr: "body" | "params" | "query" = "body",
) => {
    return async (req: Request, res: Response, next: NextFunction) => {
        try {
            await schema.parseAsync(req[requestAttr]);
            next();
        } catch (error: any) {
            if (error instanceof z.ZodError) {
                return res.status(422).json({
                    success: false,
                    schema: requestAttr,
                    error: error,
                });
            } else {
                return res.status(500).json({
                    success: false,
                    message: "Internal server error",
                });
            }
        }
    };
};

export type ValidatedSchema<T extends ZodType<any, any, any>> = z.infer<T>;