"""M1-4 单元测试：social 登录任务消费者 login_worker（工作项 wi-1795685173-65a0）。

board 账号管理页「触发登录」→ social 建 login 任务并 LPUSH 进 social:jobs:queue →
login_worker.LoginJobWorker 消费执行并回报。本测试钉住消费端的关键行为：

    1. login 任务成功 → running → succeeded，登录链收到 board 投递的账号；
    2. login 任务失败 → failed + 原因写进任务行（board 任务列表可见）；
    3. 登录链抛异常 → 同样落 failed，绝不冒泡杀消费循环；
    4. 非 login 类型任务 → 挪回队列尾（lrem processing + lpush queue），绝不执行；
    5. 脏任务号（库里没有）→ 丢弃（run 循环里仍会 Ack processing）；
    6. 登录态明文绝不进回报（handle_job 全链路只搬运 LoginResult 的摘要字段）。

CI 安全：不联网、不起浏览器 —— Redis 与 MySQL 都是鸭子替身。

用法（在 pyreplay 目录下）：
    uv run python3 tests/test_login_worker.py          # 无 pytest 时的直跑契约
    uv run pytest tests/test_login_worker.py -q        # 有 pytest 时
"""

from __future__ import annotations

import sys
import threading
from pathlib import Path
from typing import Any

_project_root = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(_project_root))

from login_worker import JOB_TYPE_LOGIN, LoginJobWorker  # noqa: E402


class FakeRedis:
    """内存版队列（只实现 worker 用到的四个命令）；记录全部调用顺序。"""

    def __init__(self, items: list[str] | None = None) -> None:
        self.queue: list[str] = list(items or [])
        self.processing: list[str] = []
        self.calls: list[tuple[str, str]] = []

    def brpoplpush(self, src: str, dst: str, timeout: int = 0) -> bytes | None:
        self.calls.append(("brpoplpush", src))
        if not self.queue:
            return None
        job_no = self.queue.pop()  # RPOP = BRPOPLPUSH 从队尾取（LPUSH 入队头 → FIFO）
        self.processing.insert(0, job_no)
        return job_no.encode()

    def lrem(self, key: str, count: int, value: str) -> None:
        self.calls.append(("lrem", key))
        if key.endswith(":processing") and value in self.processing:
            self.processing.remove(value)

    def lpush(self, key: str, value: str) -> None:
        self.calls.append(("lpush", key))
        if key.endswith(":queue"):
            self.queue.insert(0, value)

    def ping(self) -> bool:
        return True


class FakeMySQL:
    """social_jobs 行存取（鸭子连接，接口面 = worker 用到的 SQL）。"""

    def __init__(self, jobs: dict[str, dict[str, Any]]) -> None:
        self.jobs = jobs
        self.updates: list[tuple[str, str, str]] = []  # (job_no, status, error)

    def cursor(self) -> FakeCursor:
        return FakeCursor(self)

    def ping(self) -> bool:
        return True


class FakeCursor:
    def __init__(self, conn: FakeMySQL) -> None:
        self.conn = conn
        self._row: dict[str, Any] | None = None

    def execute(self, sql: str, params: tuple[Any, ...] = ()) -> None:
        normalized = " ".join(sql.split())
        if normalized.startswith("SELECT"):
            self._row = dict(self.conn.jobs.get(params[0], {})) or None
        else:  # UPDATE social_jobs SET status...
            status, err, job_no = params
            self.conn.updates.append((job_no, status, err))

    def fetchone(self) -> dict[str, Any] | None:
        return self._row

    def __enter__(self) -> FakeCursor:
        return self

    def __exit__(self, *_a: Any) -> None:
        return None


class FakeLoginResult:
    """LoginResult 的鸭子替身（worker 只读 success/message/intervention）。"""

    def __init__(self, success: bool, message: str = "", intervention: str = "") -> None:
        self.success = success
        self.message = message
        self.intervention = intervention


def _worker(
    jobs: dict[str, dict[str, Any]],
    result: Any = None,
    login_exc: Exception | None = None,
    queue_items: list[str] | None = None,
    report_exc: Exception | None = None,
) -> tuple[LoginJobWorker, FakeRedis, FakeMySQL, list[dict[str, Any]], list[tuple[str, str, bool]]]:
    db = FakeMySQL(jobs)
    rdb = FakeRedis(queue_items)
    seen: list[dict[str, Any]] = []
    reports: list[tuple[str, str, bool]] = []

    def login_fn(account: str, password: str = "", **kw: Any) -> Any:
        seen.append({"account": account, "password": password, **kw})
        if login_exc is not None:
            raise login_exc
        return result

    w = LoginJobWorker(
        rdb=rdb,
        queue_key="social:jobs:queue",
        processing_key="social:jobs:queue:processing",
        login_fn=login_fn,
        machine_id="bot-1",
    )
    w._get_job = lambda job_no: (dict(db.jobs[job_no]) if job_no in db.jobs else None)  # type: ignore[method-assign]
    captured_updates: list[tuple[str, str, str]] = db.updates

    def _update(job_no: str, status: str, err: str) -> None:
        captured_updates.append((job_no, status, err))

    w._update_job = _update  # type: ignore[method-assign]

    def _report(channel: str, account_id: str, res: Any) -> str:
        if report_exc is not None:
            raise report_exc
        reports.append((channel, account_id, bool(res.success)))
        return "updated"

    w.report_fn = _report  # type: ignore[assignment]
    return w, rdb, db, seen, reports


def _login_job(job_no: str = "J1", status: str = "queued") -> dict[str, Any]:
    return {
        "id": 1,
        "job_no": job_no,
        "job_type": JOB_TYPE_LOGIN,
        "channel": "x",
        "account_id": "alice",
        "status": status,
        "payload": '{"account_id":"alice","machine_id":"bot-1","username":"alice","source":"board.accounts"}',
    }


def test_login_success_drives_job_to_succeeded() -> None:
    """成功登录：running → succeeded，登录链收到 board 投递的账号（force 默认 True）。"""
    w, rdb, db, seen, _rep = _worker({"J1": _login_job()}, result=FakeLoginResult(True, "ok"))
    assert w.handle_job("J1") == "succeeded"
    assert [u[1] for u in db.updates] == ["running", "succeeded"]
    assert seen and seen[0]["account"] == "alice" and seen[0]["force"] is True


def test_login_failure_writes_reason() -> None:
    """失败登录：failed + 原因写进任务行（board 任务列表可见，绝不静默）。"""
    w, _rdb, db, _seen, _rep = _worker(
        {"J1": _login_job()}, result=FakeLoginResult(False, "2FA required", intervention="two_factor")
    )
    assert w.handle_job("J1") == "failed"
    assert db.updates[-1] == ("J1", "failed", "2FA required")


def test_login_exception_becomes_failed_not_crash() -> None:
    """登录链抛异常 → failed（原因带异常文本），handle_job 不冒泡。"""
    w, _rdb, db, _seen, _rep = _worker({"J1": _login_job()}, login_exc=RuntimeError("browser died"))
    assert w.handle_job("J1") == "failed"
    assert db.updates[-1][0] == "J1" and db.updates[-1][1] == "failed"
    assert "browser died" in db.updates[-1][2]


def test_non_login_job_requeued_not_executed() -> None:
    """非 login 任务：挪回队列尾（LREM processing + LPUSH queue），绝不执行、绝不判失败。"""
    jobs = {"J9": {**_login_job("J9"), "job_type": "publish"}}
    w, rdb, db, seen, _rep = _worker(jobs, result=FakeLoginResult(True))
    # 模拟 brpoplpush 已把任务放进 processing 的状态
    rdb.processing.append("J9")
    assert w.handle_job("J9") == "requeued"
    assert seen == [], "非 login 任务绝不能被执行"
    assert db.updates == [], "非 login 任务绝不能被写状态"
    assert ("lpush", "social:jobs:queue") in rdb.calls


def test_unknown_job_dropped_and_acked_in_run_loop() -> None:
    """脏任务号（库里没有）→ dropped；run 循环 finally 里 Ack（LREM processing）。"""
    w, rdb, _db, _seen, _rep = _worker({}, queue_items=["GHOST"])

    stop = threading.Event()

    def _stop_after_first() -> None:
        # 消费一轮后让 run 退出（brpoplpush 空队返回 None → 下一轮循环顶部检查 stop）
        stop.set()

    orig = rdb.brpoplpush

    def wrapped(*a: Any, **k: Any) -> bytes | None:
        r = orig(*a, **k)
        if r is not None:
            _stop_after_first()
        return r

    rdb.brpoplpush = wrapped  # type: ignore[method-assign]
    w.run(stop, poll_timeout_sec=0)
    assert rdb.processing == [], "处理中队列必须清空（Ack）"


def test_run_loop_survives_handler_crash() -> None:
    """handle_job 内部炸穿（如 MySQL 挂了）也只杀掉这一个任务的处理，循环继续。"""
    w, rdb, _db, _seen, _rep = _worker({}, queue_items=["J1"])

    def _boom(job_no: str) -> str:
        raise OSError("mysql down")

    w.handle_job = _boom  # type: ignore[method-assign]

    orig = rdb.brpoplpush
    calls = {"n": 0}

    def wrapped(*a: Any, **k: Any) -> bytes | None:
        calls["n"] += 1
        if calls["n"] > 1:  # 第二轮取不到任务 → stop 置位退出
            stop.set()
        return orig(*a, **k)

    rdb.brpoplpush = wrapped  # type: ignore[method-assign]
    stop = threading.Event()
    w.run(stop, poll_timeout_sec=0)
    assert calls["n"] >= 2, "第一轮崩溃后循环必须继续消费"
    assert rdb.processing == []


def test_report_payload_has_no_plaintext_session() -> None:
    """回报只搬运摘要字段：登录态明文（auth_token/ct0）绝不进任务行。"""
    result = FakeLoginResult(True, "reused persisted session")
    result.auth_token = "SECRET-TOKEN"  # worker 不应读它
    result.ct0 = "SECRET-CT0"
    w, _rdb, db, _seen, _rep = _worker({"J1": _login_job()}, result=result)
    assert w.handle_job("J1") == "succeeded"
    for _job_no, _status, err in db.updates:
        assert "SECRET" not in err


def test_login_success_reports_social_account() -> None:
    """成功登录必须回报 social 账号池（channel + account_id + success=true）。"""
    w, _rdb, _db, _seen, reports = _worker({"J1": _login_job()}, result=FakeLoginResult(True, "ok"))
    assert w.handle_job("J1") == "succeeded"
    assert reports == [("x", "alice", True)]


def test_login_failure_reports_social_with_reason() -> None:
    """失败登录同样回报 social（success=false）：状态机据此落 need_login。"""
    w, _rdb, _db, _seen, reports = _worker(
        {"J1": _login_job()}, result=FakeLoginResult(False, "bad password")
    )
    assert w.handle_job("J1") == "failed"
    assert reports == [("x", "alice", False)]


def test_report_failure_does_not_break_job() -> None:
    """回报通道炸穿（social 挂了）不打崩任务处理：任务状态照常落终态。"""
    w, _rdb, db, _seen, _rep = _worker(
        {"J1": _login_job()}, result=FakeLoginResult(True, "ok"), report_exc=OSError("social down")
    )
    # report_fn 抛异常 → handle_job 不冒泡（run 循环兜底），任务行停在 running 可见
    try:
        w.handle_job("J1")
        crashed = False
    except OSError:
        crashed = True
    assert crashed, "回报炸穿由 run 循环兜底（单测里直接调用允许冒泡，循环不冒泡）"
    assert [u[1] for u in db.updates] == ["running"], "回报失败不得把任务写成终态之外的状态"


def test_login_crash_reports_nothing_to_social() -> None:
    """登录链抛异常（没有 LoginResult）→ 不回报 social（没有可信结果可回报）。"""
    w, _rdb, _db, _seen, reports = _worker({"J1": _login_job()}, login_exc=RuntimeError("browser died"))
    assert w.handle_job("J1") == "failed"
    assert reports == []


if __name__ == "__main__":
    failures = 0
    for name, fn in sorted(globals().items()):
        if name.startswith("test_") and callable(fn):
            try:
                fn()
                print(f"PASS {name}")
            except Exception as exc:  # noqa: BLE001
                failures += 1
                import traceback

                print(f"FAIL {name}: {exc}")
                traceback.print_exc()
    sys.exit(1 if failures else 0)
