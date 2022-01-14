package config

import (
	"io"
	"os"
	"path"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"gopkg.in/natefinch/lumberjack.v2"
)

type SupervisorLogger struct {
	Logger *zerolog.Logger
}

func (l *SupervisorLogger) Printf(format string, v ...interface{}) {
	l.Logger.Printf(format, v...)
}
func (l *SupervisorLogger) Println(v ...interface{}) {
	l.Logger.Print(v...)
}

type LoggerConfig struct {
	// Enable console logging
	ConsoleLoggingEnabled bool

	// Enable Debug mode
	DebugModeEnabled bool

	// EncodeLogsAsJson makes the log framework log JSON
	EncodeLogsAsJson bool
	// FileLoggingEnabled makes the framework log to a file
	// the fields below can be skipped if this value is false!
	FileLoggingEnabled bool
	// Directory to log to to when filelogging is enabled
	Directory string
	// Filename is the name of the logfile which will be placed inside the directory
	Filename string
	// MaxSize the max size in MB of the logfile before it's rolled
	MaxSize int
	// MaxBackups the max number of rolled files to keep
	MaxBackups int
	// MaxAge the max age in days to keep a logfile
	MaxAge int
}

func buildLoggerConfig(debugModeEnabled bool) (*LoggerConfig, error) {
	conf := LoggerConfig{
		DebugModeEnabled: debugModeEnabled,
	}

	if v, err := GetenvBool("CONSOLE_LOGGING_ENABLED"); err != nil {
		return nil, err
	} else if v != nil {
		conf.ConsoleLoggingEnabled = *v
	} else {
		conf.ConsoleLoggingEnabled = true
	}

	if v, err := GetenvBool("ENCODE_LOGS_AS_JSON"); err != nil {
		return nil, err
	} else if v != nil {
		conf.EncodeLogsAsJson = *v
	} else {
		conf.EncodeLogsAsJson = true
	}

	if v, err := GetenvBool("FILE_LOGGING_ENABLED"); err != nil {
		return nil, err
	} else if v != nil && *v {
		if v := GetenvStr("LOGS_DIRECTORY"); v == "" {
			conf.Directory = "logs"
		} else {
			conf.Directory = v
		}

		if v := GetenvStr("LOGS_FILE_NAME"); v == "" {
			conf.Filename = "cicero.log"
		} else {
			conf.Filename = v
		}

		if v, err := GetenvInt("LOGS_MAX_SIZE"); err != nil {
			return nil, err
		} else if v != nil {
			conf.MaxSize = *v
		} else {
			conf.MaxSize = 10
		}

		if v, err := GetenvInt("LOGS_MAX_BACKUPS"); err != nil {
			return nil, err
		} else if v != nil {
			conf.MaxBackups = *v
		} else {
			conf.MaxBackups = 10
		}

		if v, err := GetenvInt("LOGS_MAX_AGE"); err != nil {
			return nil, err
		} else if v != nil {
			conf.MaxAge = *v
		} else {
			conf.MaxAge = 10
		}
	}

	return &conf, nil
}

func ConfigureLogger(debugModeEnabled bool) *zerolog.Logger {
	config, err := buildLoggerConfig(debugModeEnabled)
	if err != nil {
		log.Fatal().Err(err).Msg("can't get logger config")
	}
	var writers []io.Writer

	if config.ConsoleLoggingEnabled {
		writers = append(writers, zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})
	}
	if config.FileLoggingEnabled {
		writers = append(writers, newRollingFile(config))
	}
	mw := io.MultiWriter(writers...)

	logger := zerolog.New(mw).With().Timestamp().Logger()

	zerolog.SetGlobalLevel(zerolog.InfoLevel)
	if debugModeEnabled {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	}

	logger.Info().
		Bool("consoleLogging", config.ConsoleLoggingEnabled).
		Bool("debugMode", config.DebugModeEnabled).
		Bool("jsonLogOutput", config.EncodeLogsAsJson).
		Bool("fileLogging", config.FileLoggingEnabled).
		Str("logDirectory", config.Directory).
		Str("fileName", config.Filename).
		Int("maxSizeMB", config.MaxSize).
		Int("maxBackups", config.MaxBackups).
		Int("maxAgeInDays", config.MaxAge).
		Msg("logging configured")

	return &logger
}

func newRollingFile(config *LoggerConfig) io.Writer {
	if err := os.MkdirAll(config.Directory, 0o744); err != nil {
		log.Fatal().Err(err).Str("path", config.Directory).Msg("can't create log directory")
		return nil
	}

	return &lumberjack.Logger{
		Filename:   path.Join(config.Directory, config.Filename),
		MaxBackups: config.MaxBackups, // files
		MaxSize:    config.MaxSize,    // megabytes
		MaxAge:     config.MaxAge,     // days
	}
}
