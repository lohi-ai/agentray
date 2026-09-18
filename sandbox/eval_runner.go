package sandbox

// pythonEvalRunner is intentionally self-contained and standard-library-only.
// Protocol frames use a duplicate of the original stdout descriptor; fd 1 and
// fd 2 are redirected to per-cell drain pipes before user code runs, so a print
// or child process cannot corrupt the NDJSON control stream or fill local disk.
const pythonEvalRunner = `
from __future__ import annotations

import ast
import asyncio
import builtins
import inspect
import json
import os
import sys
import threading
import traceback

_protocol = os.fdopen(os.dup(1), "w", encoding="utf-8", errors="backslashreplace", buffering=1)
_write_lock = threading.Lock()
_loop = asyncio.new_event_loop()
asyncio.set_event_loop(_loop)
_execution_count = 0
_result_name = "__agentray_cell_result__"

def _emit(value):
    line = json.dumps(value, ensure_ascii=False, default=repr)
    with _write_lock:
        _protocol.write(line + "\n")
        _protocol.flush()

def _emit_text(kind, request_id, text):
    if not text:
        return
    step = 8192
    for start in range(0, len(text), step):
        _emit({"type": kind, "id": request_id, "data": text[start:start + step]})

def _capture_fd(kind, request_id, target_fd):
    read_fd, write_fd = os.pipe()
    os.dup2(write_fd, target_fd)
    os.close(write_fd)
    done = threading.Event()
    def drain():
        try:
            while True:
                data = os.read(read_fd, 8192)
                if not data:
                    return
                _emit({"type": kind, "id": request_id, "data": data.decode("utf-8", "replace")})
        finally:
            os.close(read_fd)
            done.set()
    threading.Thread(target=drain, daemon=True).start()
    return done

def display(*values):
    for value in values:
        try:
            print(json.dumps(value, ensure_ascii=False, indent=2, default=repr))
        except Exception:
            print(repr(value))

def _no_input(*_args, **_kwargs):
    raise RuntimeError("interactive input is not supported by AgentRay eval")

builtins.input = _no_input
_user_ns = {
    "__name__": "__main__",
    "__doc__": None,
    "__builtins__": builtins,
    "display": display,
}

def _compile_cell(source):
    flags = ast.PyCF_ALLOW_TOP_LEVEL_AWAIT
    tree = compile(source, "<cell>", "exec", flags=flags | ast.PyCF_ONLY_AST, dont_inherit=True)
    has_result = bool(tree.body and isinstance(tree.body[-1], ast.Expr))
    if has_result:
        expr = tree.body[-1]
        target = ast.Name(id=_result_name, ctx=ast.Store())
        tree.body[-1] = ast.copy_location(ast.Assign(targets=[target], value=expr.value), expr)
        ast.fix_missing_locations(tree)
    return compile(tree, "<cell>", "exec", flags=flags, dont_inherit=True), has_result

def _run_cell(source):
    compiled, has_result = _compile_cell(source)
    pending = eval(compiled, _user_ns, _user_ns)
    if inspect.isawaitable(pending):
        _loop.run_until_complete(pending)
    if not has_result:
        return None, False
    value = _user_ns.pop(_result_name, None)
    _user_ns["_"] = value
    return value, True

for _line in sys.stdin:
    try:
        _request = json.loads(_line)
    except Exception:
        continue
    if _request.get("type") == "exit":
        break
    _request_id = str(_request.get("id", ""))
    _execution_count += 1
    _status = "ok"
    _result_text = ""
    _error_text = ""
    try:
        sys.stdout.flush()
        sys.stderr.flush()
    except Exception:
        pass
    _stdout_done = _capture_fd("stdout", _request_id, 1)
    _stderr_done = _capture_fd("stderr", _request_id, 2)
    try:
        _value, _has_result = _run_cell(str(_request.get("code", "")))
        if _has_result and _value is not None:
            _result_text = repr(_value)
    except BaseException:
        _status = "error"
        _error_text = traceback.format_exc()
    try:
        sys.stdout.flush()
        sys.stderr.flush()
    except Exception:
        pass
    _devnull = os.open(os.devnull, os.O_WRONLY)
    os.dup2(_devnull, 1)
    os.dup2(_devnull, 2)
    os.close(_devnull)
    # Synchronous output drains before the done frame. A deliberately detached
    # child may retain the old pipe; do not let it pin the cell indefinitely.
    _stdout_done.wait(0.5)
    _stderr_done.wait(0.5)
    _emit_text("result", _request_id, _result_text)
    _emit_text("error", _request_id, _error_text)
    _emit({
        "type": "done",
        "id": _request_id,
        "status": _status,
        "execution_count": _execution_count,
    })

try:
    _pending = asyncio.all_tasks(_loop)
    for _task in _pending:
        _task.cancel()
    if _pending:
        _loop.run_until_complete(asyncio.gather(*_pending, return_exceptions=True))
    _loop.close()
except Exception:
    pass
`
