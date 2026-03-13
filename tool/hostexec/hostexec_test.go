//
// Tencent is pleased to support the open source community by making
// trpc-agent-go available.
//
// Copyright (C) 2025 Tencent.  All rights reserved.
//
// trpc-agent-go is licensed under the Apache License Version 2.0.
//

package hostexec

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"trpc.group/trpc-go/trpc-agent-go/tool"
)

func TestNewToolSet_Foreground(t *testing.T) {
	if _, _, err := shellSpec(); err != nil {
		t.Skip(err.Error())
	}

	set, err := NewToolSet()
	require.NoError(t, err)
	defer set.Close()

	execTool, _, _, mgr := toolSetTools(t, set)
	out, err := execTool.Call(
		context.Background(),
		mustJSON(t, map[string]any{
			"command": "echo hello",
			"yieldMs": 0,
		}),
	)
	require.NoError(t, err)

	res := out.(map[string]any)
	require.Equal(t, programStatusExited, res["status"])
	require.Contains(t, outputField(res), "hello")
	require.EqualValues(t, 0, res["exit_code"])
	require.Empty(t, mgr.sessions)
}

func TestNewToolSet_BaseDirAndRelativeWorkdir(t *testing.T) {
	if _, _, err := shellSpec(); err != nil {
		t.Skip(err.Error())
	}

	baseDir := t.TempDir()
	subDir := filepath.Join(baseDir, "sub")
	require.NoError(t, os.MkdirAll(subDir, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(subDir, "note.txt"),
		[]byte("hostexec"),
		0o644,
	))

	set, err := NewToolSet(WithBaseDir(baseDir))
	require.NoError(t, err)
	defer set.Close()

	execTool, _, _, _ := toolSetTools(t, set)
	out, err := execTool.Call(
		context.Background(),
		mustJSON(t, map[string]any{
			"command": "cat note.txt",
			"workdir": "sub",
			"yieldMs": 0,
		}),
	)
	require.NoError(t, err)

	res := out.(map[string]any)
	require.Equal(t, programStatusExited, res["status"])
	require.Contains(t, outputField(res), "hostexec")
}

func TestNewToolSet_YieldAndPoll(t *testing.T) {
	if _, _, err := shellSpec(); err != nil {
		t.Skip(err.Error())
	}

	set, err := NewToolSet(WithJobTTL(10 * time.Second))
	require.NoError(t, err)
	defer set.Close()

	execTool, _, _, mgr := toolSetTools(t, set)
	out, err := execTool.Call(
		context.Background(),
		mustJSON(t, map[string]any{
			"command": "echo start; sleep 0.2; echo end",
			"yieldMs": 10,
		}),
	)
	require.NoError(t, err)

	res := out.(map[string]any)
	require.Equal(t, programStatusRunning, res["status"])
	sessionID := res["session_id"].(string)
	require.NotEmpty(t, sessionID)

	const (
		pollDeadline = 2 * time.Second
		pollInterval = 50 * time.Millisecond
	)
	deadline := time.Now().Add(pollDeadline)
	all := outputField(res)
	for time.Now().Before(deadline) {
		poll, err := mgr.poll(sessionID, nil)
		require.NoError(t, err)
		if poll.Output != "" {
			all += "\n" + poll.Output
		}
		if poll.Status == programStatusExited {
			require.Contains(t, all, "start")
			require.Contains(t, all, "end")
			return
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("process did not exit; output: %s", all)
}

func TestNewToolSet_WriteStdin(t *testing.T) {
	if _, _, err := shellSpec(); err != nil {
		t.Skip(err.Error())
	}

	set, err := NewToolSet(WithJobTTL(10 * time.Second))
	require.NoError(t, err)
	defer set.Close()

	execTool, writeTool, _, mgr := toolSetTools(t, set)
	out, err := execTool.Call(
		context.Background(),
		mustJSON(t, map[string]any{
			"command":    `read -r line; echo got:$line`,
			"background": true,
		}),
	)
	require.NoError(t, err)

	res := out.(map[string]any)
	sessionID := res["session_id"].(string)
	require.NotEmpty(t, sessionID)

	writeOut, err := writeTool.Call(
		context.Background(),
		mustJSON(t, map[string]any{
			"session_id":     sessionID,
			"chars":          "hi",
			"append_newline": true,
		}),
	)
	require.NoError(t, err)

	all := outputField(writeOut.(map[string]any))
	all += pollUntilExited(t, mgr, sessionID)
	require.Contains(t, all, "got:hi")
}

func TestNewToolSet_WriteStdin_NoRepeatedInitialOutput(t *testing.T) {
	if _, _, err := shellSpec(); err != nil {
		t.Skip(err.Error())
	}

	set, err := NewToolSet(WithJobTTL(10 * time.Second))
	require.NoError(t, err)
	defer set.Close()

	execTool, writeTool, _, _ := toolSetTools(t, set)
	out, err := execTool.Call(
		context.Background(),
		mustJSON(t, map[string]any{
			"command": "printf 'ready\\n'; read -r line; " +
				"echo got:$line",
			"yieldMs": 100,
		}),
	)
	require.NoError(t, err)

	res := out.(map[string]any)
	require.Equal(t, programStatusRunning, res["status"])
	require.Contains(t, outputField(res), "ready")
	sessionID := res["session_id"].(string)
	require.NotEmpty(t, sessionID)

	writeOut, err := writeTool.Call(
		context.Background(),
		mustJSON(t, map[string]any{
			"session_id":     sessionID,
			"chars":          "hi",
			"append_newline": true,
		}),
	)
	require.NoError(t, err)
	require.NotContains(t, outputField(writeOut.(map[string]any)), "ready")
	require.Contains(t, outputField(writeOut.(map[string]any)), "got:hi")
}

func TestNewToolSet_KillSession(t *testing.T) {
	if _, _, err := shellSpec(); err != nil {
		t.Skip(err.Error())
	}

	set, err := NewToolSet(WithJobTTL(10 * time.Second))
	require.NoError(t, err)
	defer set.Close()

	execTool, _, killTool, mgr := toolSetTools(t, set)
	out, err := execTool.Call(
		context.Background(),
		mustJSON(t, map[string]any{
			"command":    "sleep 5",
			"background": true,
		}),
	)
	require.NoError(t, err)

	res := out.(map[string]any)
	sessionID := res["session_id"].(string)
	require.NotEmpty(t, sessionID)

	killOut, err := killTool.Call(
		context.Background(),
		mustJSON(t, map[string]any{
			"session_id": sessionID,
		}),
	)
	require.NoError(t, err)

	killRes := killOut.(map[string]any)
	require.Equal(t, true, killRes["ok"])
	require.Equal(t, sessionID, killRes["session_id"])
	_ = pollUntilExited(t, mgr, sessionID)
}

func TestNewToolSet_KillSessionRespectsContext(t *testing.T) {
	if _, _, err := shellSpec(); err != nil {
		t.Skip(err.Error())
	}

	set, err := NewToolSet(WithJobTTL(10 * time.Second))
	require.NoError(t, err)
	defer set.Close()

	execTool, _, killTool, mgr := toolSetTools(t, set)
	out, err := execTool.Call(
		context.Background(),
		mustJSON(t, map[string]any{
			"command":    "sleep 5",
			"background": true,
		}),
	)
	require.NoError(t, err)

	res := out.(map[string]any)
	sessionID := res["session_id"].(string)
	require.NotEmpty(t, sessionID)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	started := time.Now()
	killOut, err := killTool.Call(
		ctx,
		mustJSON(t, map[string]any{
			"session_id": sessionID,
		}),
	)
	require.NoError(t, err)
	require.Less(t, time.Since(started), time.Second)

	killRes := killOut.(map[string]any)
	require.Equal(t, true, killRes["ok"])
	require.Equal(t, sessionID, killRes["session_id"])
	_ = pollUntilExited(t, mgr, sessionID)
}

func TestNewToolSet_CloseKillsSessions(t *testing.T) {
	if _, _, err := shellSpec(); err != nil {
		t.Skip(err.Error())
	}

	set, err := NewToolSet(WithJobTTL(10 * time.Second))
	require.NoError(t, err)

	execTool, _, _, toolMgr := toolSetTools(t, set)
	_, err = execTool.Call(
		context.Background(),
		mustJSON(t, map[string]any{
			"command":    "sleep 5",
			"background": true,
		}),
	)
	require.NoError(t, err)
	require.NotEmpty(t, toolMgr.sessions)
	require.NoError(t, set.Close())
	require.Empty(t, toolMgr.sessions)
}

func TestNewToolSet_PTYForeground(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("pty is not supported on windows")
	}
	if _, _, err := shellSpec(); err != nil {
		t.Skip(err.Error())
	}

	set, err := NewToolSet()
	require.NoError(t, err)
	defer set.Close()

	execTool, _, _, _ := toolSetTools(t, set)
	out, err := execTool.Call(
		context.Background(),
		mustJSON(t, map[string]any{
			"command": "echo hi",
			"pty":     true,
			"yieldMs": 0,
		}),
	)
	require.NoError(t, err)

	res := out.(map[string]any)
	require.Equal(t, programStatusExited, res["status"])
	require.Contains(t, outputField(res), "hi")
}

func TestResolveWorkdir(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	wd, err := resolveWorkdir("", "/tmp/base")
	require.NoError(t, err)
	require.Equal(t, "/tmp/base", wd)

	wd, err = resolveWorkdir("~", "")
	require.NoError(t, err)
	require.Equal(t, home, wd)

	wd, err = resolveWorkdir("~/x", "")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, "x"), wd)

	wd, err = resolveWorkdir("sub", "/tmp/base")
	require.NoError(t, err)
	require.Equal(t, filepath.Join("/tmp/base", "sub"), wd)
}

func TestTools_InvalidArgs(t *testing.T) {
	set, err := NewToolSet()
	require.NoError(t, err)
	defer set.Close()

	execTool, writeTool, killTool, _ := toolSetTools(t, set)

	_, err = execTool.Call(context.Background(), []byte("{"))
	require.Error(t, err)

	_, err = writeTool.Call(context.Background(), []byte("{"))
	require.Error(t, err)

	_, err = killTool.Call(context.Background(), []byte("{"))
	require.Error(t, err)
}

func TestToolDeclarations_UseIntegerDurations(t *testing.T) {
	set, err := NewToolSet()
	require.NoError(t, err)
	defer set.Close()

	execTool, writeTool, _, _ := toolSetTools(t, set)
	execDecl := execTool.Declaration().InputSchema.Properties
	require.Equal(t, "integer", execDecl["yield_time_ms"].Type)
	require.Equal(t, "integer", execDecl["yieldMs"].Type)
	require.Equal(t, "integer", execDecl["timeout_sec"].Type)
	require.Equal(t, "integer", execDecl["timeoutSec"].Type)

	writeDecl := writeTool.Declaration().InputSchema.Properties
	require.Equal(t, "integer", writeDecl["yield_time_ms"].Type)
	require.Equal(t, "integer", writeDecl["yieldMs"].Type)
}

func TestManager_GetUnknownSession(t *testing.T) {
	mgr := newManager()

	_, err := mgr.get("missing")
	require.ErrorIs(t, err, errUnknownSession)
}

func TestNewToolSet_HugeTimeout(t *testing.T) {
	if _, _, err := shellSpec(); err != nil {
		t.Skip(err.Error())
	}

	set, err := NewToolSet()
	require.NoError(t, err)
	defer set.Close()

	execTool, _, _, _ := toolSetTools(t, set)
	hugeTimeout := int(^uint(0) >> 1)
	out, err := execTool.Call(
		context.Background(),
		mustJSON(t, map[string]any{
			"command":       "sleep 0.1; echo ok",
			"yield_time_ms": 0,
			"timeout_sec":   hugeTimeout,
		}),
	)
	require.NoError(t, err)

	res := out.(map[string]any)
	require.Equal(t, programStatusExited, res["status"])
	require.Contains(t, outputField(res), "ok")
	require.EqualValues(t, 0, res["exit_code"])
}

func TestSessionKill_IgnoresProcessDone(t *testing.T) {
	if _, _, err := shellSpec(); err != nil {
		t.Skip(err.Error())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd, err := shellCmd(ctx, "true")
	require.NoError(t, err)
	require.NoError(t, cmd.Start())

	_, err = cmd.Process.Wait()
	require.NoError(t, err)

	sess := newSession("done", "true", defaultMaxLines)
	sess.cmd = cmd
	sess.cancel = cancel

	require.NoError(t, sess.kill(context.Background(), 0))
}

func toolSetTools(
	t *testing.T,
	set tool.ToolSet,
) (
	tool.CallableTool,
	tool.CallableTool,
	tool.CallableTool,
	*manager,
) {
	t.Helper()

	typed := set.(*toolSet)
	return typed.tools[0].(tool.CallableTool),
		typed.tools[1].(tool.CallableTool),
		typed.tools[2].(tool.CallableTool),
		typed.mgr
}

func pollUntilExited(
	t *testing.T,
	mgr *manager,
	sessionID string,
) string {
	t.Helper()

	const (
		pollDeadline = 2 * time.Second
		pollInterval = 50 * time.Millisecond
	)
	deadline := time.Now().Add(pollDeadline)
	var all string
	for time.Now().Before(deadline) {
		poll, err := mgr.poll(sessionID, nil)
		require.NoError(t, err)
		if poll.Output != "" {
			all += "\n" + poll.Output
		}
		if poll.Status == programStatusExited {
			return all
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("process did not exit; output: %s", all)
	return ""
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()

	data, err := json.Marshal(value)
	require.NoError(t, err)
	return data
}

func outputField(out map[string]any) string {
	value, _ := out["output"].(string)
	return strings.TrimSpace(value)
}
