import express from "express";
import { login, register } from "../controllers/User";
import { failureHandler } from "../library/utils";
import { validate } from "../validators";
import { loginSchema, registerSchema } from "../validators/User";

const router = express.Router();

router.post("/login", validate(loginSchema), failureHandler(login));
router.post("/register", validate(registerSchema), failureHandler(register));

export default router;
