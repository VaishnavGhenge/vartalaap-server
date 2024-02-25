import winston from 'winston';

const logger = winston.createLogger({
  format: winston.format.combine(
    winston.format.colorize(),
    winston.format.timestamp(),
    winston.format.printf(({ timestamp, level, message, meta }) => {
      // return `${timestamp} [${level}]: ${message} ${meta ? JSON.stringify(meta, null, 2) : ''} \n`;
      return `${timestamp} [${level}]: ${message}`;

    })
  ),
  transports: [
    new winston.transports.Console()
  ]
});

export default logger;
