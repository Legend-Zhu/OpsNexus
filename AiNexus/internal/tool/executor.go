package tool

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/legeosoft/ainexus/internal/config"
)

// CommandExecutor 命令执行工具
type CommandExecutor struct {
	allowedCommands []string
	timeout         time.Duration
	workDir         string
}

// NewCommandExecutor 创建命令执行工具
func NewCommandExecutor(cfg config.CommandToolConfig) *CommandExecutor {
	return &CommandExecutor{
		allowedCommands: cfg.AllowedCommands,
		timeout:        cfg.Timeout,
		workDir:        cfg.WorkDir,
	}
}

func (e *CommandExecutor) Name() string {
	return "command_executor"
}

func (e *CommandExecutor) Description() string {
	return "Execute a shell command and return the output. Use this tool to run system commands, scripts, or programs."
}

func (e *CommandExecutor) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{
				"type":        "string",
				"description": "The command to execute",
			},
			"work_dir": map[string]any{
				"type":        "string",
				"description": "Working directory for the command (optional, overrides default)",
			},
			"timeout": map[string]any{
				"type":        "number",
				"description": "Timeout in seconds (optional, overrides default)",
			},
		},
		"required": []string{"command"},
	}
}

func (e *CommandExecutor) Execute(ctx context.Context, params map[string]any) (ToolResult, error) {
	command, ok := params["command"].(string)
	if !ok || command == "" {
		return NewErrorResult("parameter 'command' is required and must be a string"), nil
	}

	// 检查命令白名单
	if len(e.allowedCommands) > 0 {
		cmdName := extractCommandName(command)
		allowed := false
		for _, ac := range e.allowedCommands {
			if cmdName == ac {
				allowed = true
				break
			}
		}
		if !allowed {
			return NewErrorResult(fmt.Sprintf("command %q is not in the allowed list", cmdName)), nil
		}
	}

	// 确定工作目录
	workDir := e.workDir
	if wd, ok := params["work_dir"].(string); ok && wd != "" {
		workDir = wd
	}

	// 确定超时
	timeout := e.timeout
	if t, ok := params["timeout"].(float64); ok && t > 0 {
		timeout = time.Duration(t) * time.Second
	}

	// 创建带超时的上下文
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(execCtx, "cmd", "/c", command)
	} else {
		cmd = exec.CommandContext(execCtx, "sh", "-c", command)
	}
	cmd.Dir = workDir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()

	result := fmt.Sprintf("Command: %s\n", command)
	result += fmt.Sprintf("Exit Code: ")
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			result += fmt.Sprintf("%d\n", exitErr.ExitCode())
		} else {
			result += fmt.Sprintf("error: %s\n", err.Error())
		}
	} else {
		result += "0\n"
	}

	if stdout.Len() > 0 {
		result += fmt.Sprintf("\nStdout:\n%s", stdout.String())
	}
	if stderr.Len() > 0 {
		result += fmt.Sprintf("\nStderr:\n%s", stderr.String())
	}

	if err != nil && execCtx.Err() == context.DeadlineExceeded {
		return NewErrorResult(fmt.Sprintf("Command timed out after %s\n%s", timeout, result)), nil
	}

	return NewTextResult(result), nil
}

// extractCommandName 从命令字符串中提取命令名
func extractCommandName(command string) string {
	parts := strings.Fields(command)
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}
