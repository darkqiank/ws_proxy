package common

import (
	"fmt"
	"log"
	"os"
	"time"
)

var (
	// InfoLogger 信息日志
	InfoLogger *log.Logger
	// ErrorLogger 错误日志
	ErrorLogger *log.Logger
)

func init() {
	// 初始化日志记录器
	InfoLogger = log.New(os.Stdout, "[INFO] ", log.Ldate|log.Ltime)
	ErrorLogger = log.New(os.Stderr, "[ERROR] ", log.Ldate|log.Ltime)
}

// Info 输出信息日志
func Info(format string, v ...interface{}) {
	InfoLogger.Output(2, fmt.Sprintf(format, v...))
}

// Error 输出错误日志
func Error(format string, v ...interface{}) {
	ErrorLogger.Output(2, fmt.Sprintf(format, v...))
}

// FormatTime 格式化当前时间
func FormatTime() string {
	return time.Now().Format("2006-01-02 15:04:05")
}
