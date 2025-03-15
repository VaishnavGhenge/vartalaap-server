import express from "express";
import { createMeet, joinMeet } from "../controllers/Meet";
import { failureHandler } from "../library/utils";

const router = express.Router();

router.get("/join", failureHandler(joinMeet));
router.post("/create", failureHandler(createMeet));

export default router;
