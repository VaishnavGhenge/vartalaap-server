import express from "express";
import { createMeet, joinMeet } from "../controllers/Meet";
import { checkAuthToken } from "../validators/Auth";
import { failureHandler } from "../library/utils";

const router = express.Router();

// router.use(checkAuthToken);

router.get("/join", failureHandler(joinMeet));
router.post("/create", failureHandler(createMeet));

export default router;
