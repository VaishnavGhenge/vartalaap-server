import express from "express";
import { createMeet, joinMeet } from "../controllers/Meet";
import { checkAuthToken } from "../validators/Auth";

const router = express.Router();

// router.use(checkAuthToken);

router.get("/join", joinMeet);
router.post("/create", createMeet);

export default router;