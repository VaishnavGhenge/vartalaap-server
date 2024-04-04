import express from "express";
import { createMeet, joinMeet } from "../controllers/Meet";

const router = express.Router();

router.get("/join", joinMeet);
router.post("/create", createMeet);

export default router;