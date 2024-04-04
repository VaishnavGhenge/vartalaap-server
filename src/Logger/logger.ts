import winston from 'winston';

const logger = winston.createLogger({
  format: winston.format.combine(
      winston.format.colorize(),
      winston.format.timestamp(),
      winston.format.printf(({ timestamp, level, message, meta }) => {
        let logMessage = `${timestamp} [${level}]: ${message}`;
        // Apply color to log message based on log level
        switch (level) {
          case 'error':
            logMessage = `\x1b[31m${logMessage}\x1b[0m`; // Red color for error messages
            break;
          case 'warn':
            logMessage = `\x1b[33m${logMessage}\x1b[0m`; // Yellow color for warning messages
            break;
          default:
            // No additional color for other log levels
            break;
        }
        return logMessage;
      })
  ),
  transports: [
    new winston.transports.Console()
  ]
});

export default logger;

