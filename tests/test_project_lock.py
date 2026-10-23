"""Synthetic coordination checks for the shared Go/Python maintenance lease."""
from __future__ import annotations

import base64
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager, ExitStack
import errno
from pathlib import Path
import os
import selectors
import subprocess
import threading
import time

import pytest

from bb_assistant.caido import HistoryPage, RequestMetadataPage
from bb_assistant.domain import CaidoExchangeRef, CaidoProjectRef, HttpExchange
from bb_assistant.ingest import sync_live_project, sync_project
from bb_assistant.storage import ProjectStore


@pytest.fixture(scope="module")
def go_lock_helper(tmp_path_factory):
    output = tmp_path_factory.mktemp("project-lock-go-helper") / "projectlock.test"
    source_root = Path(__file__).resolve().parents[1]
    subprocess.run(
        ["go", "test", "-c", "-o", str(output), "./internal/projectlock"],
        cwd=source_root,
        check=True,
        capture_output=True,
        text=True,
        timeout=60,
    )
    return output


@contextmanager
def go_process(binary: Path, directory: Path):
    process = subprocess.Popen(
        [str(binary), "-test.run=^TestProjectLockProcessHelper$"],
        env={"BB_PROJECTLOCK_HELPER": "1", "BB_PROJECTLOCK_DIRECTORY": str(directory)},
        stdin=subprocess.PIPE,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    try:
        yield process
    finally:
        if process.poll() is None:
            try:
                process.stdin.write("release\n")
                process.stdin.flush()
            except BrokenPipeError:
                pass
        try:
            _, stderr = process.communicate(timeout=3)
        except subprocess.TimeoutExpired:
            process.kill()
            _, stderr = process.communicate(timeout=3)
        assert process.returncode == 0, stderr


def go_ready(process, timeout=3):
    with selectors.DefaultSelector() as selector:
        selector.register(process.stdout, selectors.EVENT_READ)
        return bool(selector.select(timeout=timeout))


@contextmanager
def go_holder(binary: Path, directory: Path):
    with go_process(binary, directory) as process:
        assert go_ready(process), "Go lock helper did not signal readiness"
        assert process.stdout.readline() == "locked\n", "Go lock helper did not acquire lease"
        yield process


def maintenance_lock(*args, **kwargs):
    from bb_assistant.project_lock import project_maintenance_lock

    return project_maintenance_lock(*args, **kwargs)


def maintenance_read_lock(*args, **kwargs):
    from bb_assistant.project_lock import project_maintenance_read_lock

    return project_maintenance_read_lock(*args, **kwargs)


class EmptySyntheticSource:
    def __init__(self):
        self.entered = threading.Event()

    def list_history(self, project, **kwargs):
        self.entered.set()
        return HistoryPage((), None, False)

    def list_request_page(self, project, **kwargs):
        self.entered.set()
        return RequestMetadataPage((), None, False)

    def fetch_history_page(self, project, page, **kwargs):
        return HistoryPage((), None, False)


@pytest.mark.parametrize("mode", ["history", "live"])
def test_python_ingest_waits_for_existing_go_maintenance_lease(
    mode, tmp_path, monkeypatch, go_lock_helper,
):
    monkeypatch.setenv("BB_ASSISTANT_VAULT_KEY", base64.urlsafe_b64encode(bytes(32)).decode("ascii"))
    project = CaidoProjectRef("synthetic-instance", "synthetic-project")
    store = ProjectStore(project, root=tmp_path)
    source = EmptySyntheticSource()
    sync = sync_project if mode == "history" else sync_live_project
    with ThreadPoolExecutor(max_workers=1) as executor:
        with go_holder(go_lock_helper, store.directory):
            future = executor.submit(sync, source, project, store=store)
            assert not source.entered.wait(0.15), "Python ingest bypassed the active Go maintenance lease"
        receipt = future.result(timeout=3)
    assert receipt.state == ("done" if mode == "history" else "idle")
    # A watch command sleeps only after this cycle returns. Go can already
    # acquire here, before a subsequent Python cycle starts.
    with go_holder(go_lock_helper, store.directory):
        pass


def test_python_lease_blocks_go_until_released(tmp_path, go_lock_helper):
    with ExitStack() as processes:
        with maintenance_lock(tmp_path):
            process = processes.enter_context(go_process(go_lock_helper, tmp_path))
            assert not go_ready(process, timeout=0.15), "Go bypassed the active Python lease"
        assert go_ready(process), "Go did not acquire after Python released"
        assert process.stdout.readline() == "locked\n"


def test_python_writer_lease_blocks_python_readers(tmp_path):
    with maintenance_lock(tmp_path):
        with pytest.raises(TimeoutError):
            with maintenance_read_lock(tmp_path, timeout=0.05):
                pytest.fail("read lease must wait for an active writer")


def test_go_writer_lease_blocks_python_readers(tmp_path, go_lock_helper):
    with go_holder(go_lock_helper, tmp_path):
        with pytest.raises(TimeoutError):
            with maintenance_read_lock(tmp_path, timeout=0.05):
                pytest.fail("Python read lease bypassed active Go writer")


@pytest.mark.parametrize("mode", ["history", "live"])
def test_ingest_initializes_project_store_after_acquiring_maintenance_lock(
    mode, tmp_path, monkeypatch,
):
    import bb_assistant.ingest as ingest

    monkeypatch.setenv("BB_ASSISTANT_DATA_HOME", str(tmp_path / "runtime"))
    monkeypatch.setenv("BB_ASSISTANT_VAULT_KEY", base64.urlsafe_b64encode(bytes(32)).decode("ascii"))
    project = CaidoProjectRef("synthetic-instance", f"init-under-lock-{mode}")
    source = EmptySyntheticSource()
    real_lock = ingest.project_maintenance_lock
    lock_held = False
    original_initialize = ProjectStore._initialize

    @contextmanager
    def observed_lock(*args, **kwargs):
        nonlocal lock_held
        with real_lock(*args, **kwargs):
            lock_held = True
            try:
                yield
            finally:
                lock_held = False

    def initialize_while_locked(store):
        assert lock_held, "ProjectStore initialized its SQLite schema before acquiring the lease"
        original_initialize(store)

    monkeypatch.setattr(ingest, "project_maintenance_lock", observed_lock)
    monkeypatch.setattr(ProjectStore, "_initialize", initialize_while_locked)
    sync = ingest.sync_project if mode == "history" else ingest.sync_live_project
    receipt = sync(source, project, page_delay_seconds=0) if mode == "history" else sync(source, project)
    assert receipt.state in {"done", "idle"}


def test_backfill_failure_receipt_initializes_project_store_under_lock(tmp_path, monkeypatch):
    import bb_assistant.ingest as ingest

    monkeypatch.setenv("BB_ASSISTANT_DATA_HOME", str(tmp_path / "runtime"))
    monkeypatch.setenv("BB_ASSISTANT_VAULT_KEY", base64.urlsafe_b64encode(bytes(32)).decode("ascii"))
    project = CaidoProjectRef("synthetic-instance", "fallback-init-under-lock")
    real_lock = ingest.project_maintenance_lock
    lock_held = False
    original_initialize = ProjectStore._initialize

    class FailingSyntheticSource(EmptySyntheticSource):
        def list_history(self, project, **kwargs):
            raise RuntimeError("synthetic source unavailable")

        def list_projects(self):
            return (project,)

    @contextmanager
    def observed_lock(*args, **kwargs):
        nonlocal lock_held
        with real_lock(*args, **kwargs):
            lock_held = True
            try:
                yield
            finally:
                lock_held = False

    def initialize_while_locked(store):
        assert lock_held, "fallback ProjectStore initialization bypassed the maintenance lease"
        original_initialize(store)

    monkeypatch.setattr(ingest, "project_maintenance_lock", observed_lock)
    monkeypatch.setattr(ProjectStore, "_initialize", initialize_while_locked)
    receipts = ingest.backfill_all_projects(FailingSyntheticSource(), page_delay_seconds=0)
    assert len(receipts) == 1
    assert receipts[0].state == "partial"
    assert receipts[0].problem == "RuntimeError"


def test_read_only_project_store_does_not_migrate_or_write(tmp_path):
    import sqlite3

    project = CaidoProjectRef("synthetic-instance", "readonly-store")
    writable = ProjectStore(project, root=tmp_path)
    connection = sqlite3.connect(writable.db_path)
    try:
        connection.execute("PRAGMA user_version = 4")
    finally:
        connection.close()
    writable.db_path.chmod(0o400)
    old_mtime = writable.db_path.stat().st_mtime_ns
    registry = tmp_path / "projects" / "registry.json"
    old_registry = registry.read_bytes()
    old_entries = {path.name for path in writable.directory.iterdir()}

    readonly = ProjectStore(project, root=tmp_path, read_only=True)
    with readonly.connect() as connection:
        assert connection.execute("PRAGMA user_version").fetchone()[0] == 4
        with pytest.raises(sqlite3.OperationalError):
            connection.execute("INSERT INTO project_meta(key,value) VALUES('x','y')")

    assert readonly.db_path.stat().st_mode & 0o777 == 0o400
    assert readonly.db_path.stat().st_mtime_ns == old_mtime
    assert registry.read_bytes() == old_registry
    assert {path.name for path in writable.directory.iterdir()} == old_entries


def test_writable_project_store_migrates_v5_projection_job_ledger(tmp_path):
    import sqlite3

    project = CaidoProjectRef("synthetic-instance", "projection-job-v5-migration")
    old_store = ProjectStore(project, root=tmp_path)
    with sqlite3.connect(old_store.db_path) as connection:
        connection.execute("DROP TABLE IF EXISTS projection_jobs")
        connection.execute("DROP TABLE IF EXISTS capture_revision_registry")
        connection.execute(
            "INSERT INTO captures(observation_id,caido_instance_id,caido_project_id,request_id,"
            "request_sha256,request_size,request_vault_ref) VALUES(?,?,?,?,?,?,?)",
            ("obs-v5", "synthetic-instance", "projection-job-v5-migration", "req-v5", "a" * 64, 12, "vault/ref"),
        )
        connection.execute(
            "INSERT INTO capture_revisions(revision_id,observation_id,request_sha256,request_size,"
            "request_vault_ref,first_seen_at) VALUES(?,?,?,?,?,?)",
            ("b" * 64, "obs-v5", "a" * 64, 12, "vault/ref", "2026-10-23T12:00:00Z"),
        )
        connection.execute("PRAGMA user_version = 5")

    upgraded = ProjectStore(project, root=tmp_path)
    with upgraded.connect() as connection:
        assert connection.execute("PRAGMA user_version").fetchone()[0] == 6
        columns = {
            row["name"] for row in connection.execute("PRAGMA table_info(projection_jobs)").fetchall()
        }
        revision = connection.execute(
            "SELECT observation_id,source_kind FROM capture_revision_registry WHERE revision_id=?",
            ("b" * 64,),
        ).fetchone()
    assert {
        "job_id",
        "revision_id",
        "payload_json",
        "payload_sha256",
        "state",
        "attempt_count",
        "next_attempt_at",
    } <= columns
    assert tuple(revision) == ("obs-v5", "historical")


def test_python_history_writer_registers_new_capture_revision(tmp_path):
    project = CaidoProjectRef("synthetic-instance", "python-revision-registry")
    store = ProjectStore(project, root=tmp_path)
    exchange = HttpExchange(
        ref=CaidoExchangeRef(project, "request-1"),
        captured_at="2026-10-23T12:00:00Z",
        request_raw=b"GET / HTTP/1.1\r\nHost: example.test\r\n\r\n",
        response_raw=None,
        method="GET",
        scheme="http",
        host="example.test",
        port=80,
        target="/",
    )

    receipt = store.save_exchange(exchange, key=bytes(32))

    with store.connect() as connection:
        registered = connection.execute(
            "SELECT observation_id,source_kind FROM capture_revision_registry WHERE revision_id=?",
            (receipt["revision_id"],),
        ).fetchone()
    assert tuple(registered) == (exchange.ref.observation_id, "historical")


def test_writable_project_store_rejects_unknown_schema_without_mutating_it(tmp_path):
    import sqlite3

    project = CaidoProjectRef("synthetic-instance", "unsupported-schema-is-untouched")
    store = ProjectStore(project, root=tmp_path)
    with sqlite3.connect(store.db_path) as connection:
        connection.execute("DROP TABLE projection_jobs")
        connection.execute("PRAGMA user_version = 99")

    with pytest.raises(RuntimeError, match="Unsupported project database schema version 99"):
        ProjectStore(project, root=tmp_path)

    with sqlite3.connect(store.db_path) as connection:
        assert connection.execute("PRAGMA user_version").fetchone()[0] == 99
        assert connection.execute(
            "SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='projection_jobs'"
        ).fetchone()[0] == 0


def test_read_only_tool_startup_does_not_create_registry_or_runtime_directories(tmp_path):
    from bb_assistant.registry import load_project_refs

    assert load_project_refs(tmp_path) == ()
    assert not (tmp_path / "projects").exists()


def test_read_only_facade_rejects_writable_stores(tmp_path):
    from bb_assistant.kb.index import KBIndex
    from bb_assistant.tools import ReadOnlyAssistantTools

    project = CaidoProjectRef("synthetic-instance", "facade-requires-readonly-store")
    store = ProjectStore(project, root=tmp_path)
    kb = KBIndex(tmp_path / "kb" / "index.sqlite3")
    with pytest.raises(ValueError, match="read-only project stores"):
        ReadOnlyAssistantTools({project.storage_key: store}, kb)


def test_read_only_facade_rejects_writable_kb_index(tmp_path):
    from bb_assistant.kb.index import KBIndex
    from bb_assistant.tools import ReadOnlyAssistantTools

    writable_kb = KBIndex(tmp_path / "kb" / "index.sqlite3")
    with pytest.raises(ValueError, match="read-only KB index"):
        ReadOnlyAssistantTools({}, writable_kb)


def test_read_only_behavior_graph_does_not_migrate_or_write(tmp_path):
    import sqlite3

    from bb_assistant.behavior import BehaviorGraph

    project = CaidoProjectRef("synthetic-instance", "readonly-behavior-graph")
    path = tmp_path / "behavior.sqlite3"
    with BehaviorGraph(path, project.storage_key):
        pass
    path.chmod(0o400)
    old_mtime = path.stat().st_mtime_ns
    parent_entries = {item.name for item in path.parent.iterdir()}

    with BehaviorGraph(path, project.storage_key, read_only=True) as graph:
        assert graph.snapshot()["counts"] == {"sources": 0, "entities": 0, "assertions": 0, "cards": 0}
        with pytest.raises(sqlite3.OperationalError):
            graph._connection().execute("CREATE TABLE unexpected(value TEXT)")

    assert path.stat().st_mode & 0o777 == 0o400
    assert path.stat().st_mtime_ns == old_mtime
    assert {item.name for item in path.parent.iterdir()} == parent_entries


def test_read_only_tool_startup_opens_all_indexes_without_migration(tmp_path):
    import sqlite3

    from bb_assistant.behavior import BehaviorGraph
    from bb_assistant.kb.index import KBIndex
    from bb_assistant.registry import registry_path
    from bb_assistant.tools import ReadOnlyAssistantTools

    project = CaidoProjectRef("synthetic-instance", "readonly-tool-startup")
    store = ProjectStore(project, root=tmp_path)
    connection = sqlite3.connect(store.db_path)
    try:
        connection.execute("PRAGMA user_version = 4")
    finally:
        connection.close()
    with BehaviorGraph(store.behavior_graph_path, project.storage_key):
        pass
    kb_path = tmp_path / "kb" / "index.sqlite3"
    kb = KBIndex(kb_path)

    store.db_path.chmod(0o400)
    store_mtime = store.db_path.stat().st_mtime_ns
    registry = registry_path(tmp_path)
    registry_mtime = registry.stat().st_mtime_ns
    kb_path.chmod(0o400)
    kb_mtime = kb_path.stat().st_mtime_ns
    behavior_mtime = store.behavior_graph_path.stat().st_mtime_ns
    project_files = {path.name for path in store.directory.iterdir()}

    tools = ReadOnlyAssistantTools.from_data_root(tmp_path)
    tools.call("bb_list_projects", {})
    tools.call("bb_kb_stats", {})
    assert tools._stores[project.storage_key].read_only
    assert tools._kb.read_only
    assert store.db_path.stat().st_mode & 0o777 == 0o400
    assert store.db_path.stat().st_mtime_ns == store_mtime
    assert registry.stat().st_mtime_ns == registry_mtime
    assert kb_path.stat().st_mode & 0o777 == 0o400
    assert kb_path.stat().st_mtime_ns == kb_mtime
    assert store.behavior_graph_path.stat().st_mtime_ns == behavior_mtime
    assert {path.name for path in store.directory.iterdir()} == project_files
    connection = sqlite3.connect(store.db_path)
    try:
        assert connection.execute("PRAGMA user_version").fetchone()[0] == 4
    finally:
        connection.close()


def test_read_only_project_store_refuses_uncheckpointed_wal_without_creating_files(tmp_path):
    import sqlite3

    project = CaidoProjectRef("synthetic-instance", "readonly-uncheckpointed-wal")
    store = ProjectStore(project, root=tmp_path)
    writer = sqlite3.connect(store.db_path)
    writer.execute("CREATE TABLE wal_probe(value TEXT)")
    writer.execute("INSERT INTO wal_probe(value) VALUES('synthetic')")
    writer.commit()
    wal_path = Path(str(store.db_path) + "-wal")
    assert wal_path.exists() and wal_path.stat().st_size > 0
    entries = {path.name for path in store.directory.iterdir()}
    readonly = ProjectStore(project, root=tmp_path, read_only=True)
    try:
        with pytest.raises(ValueError, match="WAL requires a writable checkpoint"):
            with readonly.connect():
                pytest.fail("read-only access must not silently ignore an uncheckpointed WAL")
        assert {path.name for path in store.directory.iterdir()} == entries
    finally:
        writer.close()


def test_writable_project_store_constructor_waits_for_go_maintenance_lease(
    tmp_path, go_lock_helper,
):
    project = CaidoProjectRef("synthetic-instance", "constructor-lock")
    directory = ProjectStore.maintenance_directory(project, tmp_path)
    database = directory / "graph.sqlite3"
    registry = tmp_path / "projects" / "registry.json"
    with ThreadPoolExecutor(max_workers=1) as executor:
        with go_holder(go_lock_helper, directory):
            future = executor.submit(ProjectStore, project, root=tmp_path)
            time.sleep(0.1)
            assert not future.done(), "writable constructor bypassed the active Go lease"
            assert not database.exists(), "constructor initialized SQLite before acquiring the lease"
            assert not registry.exists(), "constructor wrote the registry before acquiring the lease"
        store = future.result(timeout=3)
    assert store.db_path.is_file()
    assert registry.is_file()


def test_project_registry_reads_and_replaces_under_catalog_lease(tmp_path, monkeypatch):
    from bb_assistant import registry
    from bb_assistant.project_lock import ProjectLockTimeout, project_maintenance_lock

    projects = tmp_path / "projects"
    projects.mkdir(mode=0o700)
    real_load = registry.load_project_refs
    lock_observed = False

    def assert_catalog_lock(root=None):
        nonlocal lock_observed
        with pytest.raises(ProjectLockTimeout):
            with project_maintenance_lock(projects, timeout=0.03):
                pass
        lock_observed = True
        return real_load(root)

    monkeypatch.setattr(registry, "load_project_refs", assert_catalog_lock)
    first = CaidoProjectRef("synthetic-instance", "catalog-first")
    second = CaidoProjectRef("synthetic-instance", "catalog-second")
    registry.register_project(first, tmp_path)
    registry.register_project(second, tmp_path)
    assert lock_observed
    assert {ref.project_id for ref in real_load(tmp_path)} == {"catalog-first", "catalog-second"}


@pytest.mark.parametrize("kind", ["project", "behavior", "kb"])
def test_read_only_index_refuses_nonempty_rollback_journal_without_writes(tmp_path, kind):
    from bb_assistant.behavior import BehaviorGraph
    from bb_assistant.kb.index import KBIndex

    project = CaidoProjectRef("synthetic-instance", f"hot-journal-{kind}")
    if kind == "project":
        writable = ProjectStore(project, root=tmp_path)
        database = writable.db_path
        readonly = lambda: ProjectStore(project, root=tmp_path, read_only=True)
    elif kind == "behavior":
        database = tmp_path / "behavior.sqlite3"
        with BehaviorGraph(database, project.storage_key):
            pass
        readonly = lambda: BehaviorGraph(database, project.storage_key, read_only=True)
    else:
        database = tmp_path / "kb" / "index.sqlite3"
        KBIndex(database)
        readonly = lambda: KBIndex(database, read_only=True)
    journal = Path(str(database) + "-journal")
    journal.write_bytes(b"synthetic hot rollback journal")
    entries = {item.name for item in database.parent.iterdir()}
    with pytest.raises((ValueError, RuntimeError), match="journal|rollback"):
        value = readonly()
        if kind in {"project", "kb"}:
            with value.connect():
                pytest.fail(f"read-only {kind} access must refuse a rollback journal")
    assert {item.name for item in database.parent.iterdir()} == entries


def test_read_only_kb_refuses_uncheckpointed_wal_without_sidecars(tmp_path):
    import sqlite3
    from bb_assistant.kb.index import KBIndex

    database = tmp_path / "kb" / "index.sqlite3"
    KBIndex(database)
    writer = sqlite3.connect(database)
    writer.execute("PRAGMA journal_mode=WAL")
    writer.execute("PRAGMA wal_autocheckpoint=0")
    writer.execute("INSERT INTO kb_meta(key,value) VALUES('synthetic','wal')")
    writer.commit()
    wal = Path(str(database) + "-wal")
    assert wal.is_file() and wal.stat().st_size > 0
    entries = {item.name for item in database.parent.iterdir()}
    try:
        readonly = KBIndex(database, read_only=True)
        with pytest.raises(ValueError, match="WAL requires a writable checkpoint"):
            with readonly.connect():
                pytest.fail("read-only KB access must reject an uncheckpointed WAL")
        assert {item.name for item in database.parent.iterdir()} == entries
    finally:
        writer.close()


def test_read_only_kb_lease_blocks_snapshot_replacement(tmp_path):
    from bb_assistant.kb.index import KBIndex

    database = tmp_path / "kb" / "index.sqlite3"
    writable = KBIndex(database)
    readonly = KBIndex(database, read_only=True)
    with ThreadPoolExecutor(max_workers=1) as executor:
        with readonly.connect():
            future = executor.submit(writable.replace_snapshot, (), "a" * 64)
            time.sleep(0.1)
            assert not future.done(), "KB writer bypassed the active shared read lease"
        assert future.result(timeout=3) == 0


def test_python_wait_timeout_on_go_lease(tmp_path, go_lock_helper):
    with go_holder(go_lock_helper, tmp_path):
        with pytest.raises(TimeoutError):
            with maintenance_lock(tmp_path, timeout=0.05):
                pytest.fail("Python lease bypassed Go holder")


@pytest.mark.parametrize("mode", ["history", "live"])
def test_failed_ingest_cycle_releases_lease(mode, tmp_path, monkeypatch, go_lock_helper):
    monkeypatch.setenv("BB_ASSISTANT_VAULT_KEY", base64.urlsafe_b64encode(bytes(32)).decode("ascii"))
    project = CaidoProjectRef("synthetic-instance", "failed-project")
    store = ProjectStore(project, root=tmp_path)

    class FailedSource(EmptySyntheticSource):
        def list_history(self, project, **kwargs):
            raise RuntimeError("synthetic source unavailable")

        def list_request_page(self, project, **kwargs):
            raise RuntimeError("synthetic source unavailable")

    if mode == "history":
        with pytest.raises(RuntimeError, match="synthetic source unavailable"):
            sync_project(FailedSource(), project, store=store)
    else:
        receipt = sync_live_project(FailedSource(), project, store=store)
        assert receipt.state == "partial"
    with go_holder(go_lock_helper, store.directory):
        pass


def test_history_lock_failure_does_not_write_unlocked_fallback_progress(tmp_path, monkeypatch):
    import bb_assistant.ingest as ingest
    from bb_assistant.project_lock import ProjectLockTimeout

    project = CaidoProjectRef("synthetic-instance", "busy-project")
    store = ProjectStore(project, root=tmp_path)
    monkeypatch.setenv("BB_ASSISTANT_DATA_HOME", str(tmp_path))
    source = EmptySyntheticSource()
    source.list_projects = lambda: (project,)

    @contextmanager
    def busy_lock(*args, **kwargs):
        raise ProjectLockTimeout("synthetic maintenance busy")
        yield

    monkeypatch.setattr(ingest, "project_maintenance_lock", busy_lock)
    receipts = ingest.backfill_all_projects(source)
    assert len(receipts) == 1
    assert receipts[0].state == "partial"
    assert receipts[0].problem == "ProjectLockTimeout"
    assert not source.entered.is_set()
    assert store.sync_progress(ingest._SOURCE_KEY) is None


def test_history_failure_receipt_survives_fallback_lock_contention(tmp_path, monkeypatch):
    import bb_assistant.ingest as ingest
    from bb_assistant.project_lock import ProjectLockTimeout

    project = CaidoProjectRef("synthetic-instance", "fallback-busy-project")
    store = ProjectStore(project, root=tmp_path)
    monkeypatch.setenv("BB_ASSISTANT_DATA_HOME", str(tmp_path))
    source = EmptySyntheticSource()
    source.list_projects = lambda: (project,)

    def failed_sync(*args, **kwargs):
        raise RuntimeError("synthetic source failed")

    def busy_lock(*args, **kwargs):
        raise ProjectLockTimeout("synthetic maintenance busy")

    monkeypatch.setattr(ingest, "sync_project", failed_sync)
    monkeypatch.setattr(ingest, "project_maintenance_lock", busy_lock)
    receipts = ingest.backfill_all_projects(source)
    assert len(receipts) == 1
    assert receipts[0].state == "partial"
    assert receipts[0].problem == "ProjectLockTimeout"
    assert store.sync_progress(ingest._SOURCE_KEY) is None


def test_live_tail_reports_lock_contention_and_continues_to_next_project(tmp_path, monkeypatch):
    import bb_assistant.ingest as ingest
    from bb_assistant.project_lock import ProjectLockTimeout

    monkeypatch.setenv("BB_ASSISTANT_VAULT_KEY", base64.urlsafe_b64encode(bytes(32)).decode("ascii"))
    monkeypatch.setenv("BB_ASSISTANT_DATA_HOME", str(tmp_path))
    first = CaidoProjectRef("synthetic-instance", "busy-live-project")
    second = CaidoProjectRef("synthetic-instance", "available-live-project")
    ProjectStore(first, root=tmp_path)
    ProjectStore(second, root=tmp_path)
    source = EmptySyntheticSource()
    source.list_projects = lambda: (first, second)

    @contextmanager
    def busy_first_project(directory, **kwargs):
        if directory == ProjectStore.maintenance_directory(first):
            raise ProjectLockTimeout("synthetic maintenance busy")
        yield

    monkeypatch.setattr(ingest, "project_maintenance_lock", busy_first_project)
    receipts = ingest.tail_all_projects(source)

    assert [(receipt.project_key, receipt.state, receipt.problem) for receipt in receipts] == [
        (first.storage_key, "partial", "ProjectLockTimeout"),
        (second.storage_key, "idle", None),
    ]
    assert source.entered.is_set()


def test_live_tail_does_not_swallow_non_lock_failures(tmp_path, monkeypatch):
    import bb_assistant.ingest as ingest

    project = CaidoProjectRef("synthetic-instance", "failed-live-project")
    ProjectStore(project, root=tmp_path)
    source = EmptySyntheticSource()
    source.list_projects = lambda: (project,)

    @contextmanager
    def unexpected_failure(*args, **kwargs):
        raise RuntimeError("synthetic unexpected failure")
        yield

    monkeypatch.setattr(ingest, "project_maintenance_lock", unexpected_failure)
    with pytest.raises(RuntimeError, match="synthetic unexpected failure"):
        ingest.tail_all_projects(source)


def test_python_lock_permissions_retention_and_exception_release(tmp_path):
    with pytest.raises(RuntimeError, match="synthetic body failure"):
        with maintenance_lock(tmp_path):
            assert (tmp_path / ".maintenance.lock").stat().st_mode & 0o7777 == 0o600
            raise RuntimeError("synthetic body failure")
    assert (tmp_path / ".maintenance.lock").exists()
    with maintenance_lock(tmp_path):
        with pytest.raises(TimeoutError):
            with maintenance_lock(tmp_path, timeout=0.03):
                pytest.fail("lease must not be reentrant")


def test_python_independent_projects_do_not_block(tmp_path):
    other = tmp_path / "other"
    other.mkdir(mode=0o700)
    with maintenance_lock(tmp_path), maintenance_lock(other):
        pass


def test_wait_cancel_event_interrupts_promptly(tmp_path):
    canceled = threading.Event()
    with maintenance_lock(tmp_path):
        timer = threading.Timer(0.03, canceled.set)
        timer.start()
        start = time.monotonic()
        try:
            with pytest.raises(InterruptedError):
                with maintenance_lock(tmp_path, cancel_event=canceled):
                    pytest.fail("canceled waiter acquired")
        finally:
            timer.cancel()
            timer.join()
        assert time.monotonic() - start < 1


def test_precanceled_wait_does_not_create_lock_file(tmp_path):
    canceled = threading.Event()
    canceled.set()
    with pytest.raises(InterruptedError):
        with maintenance_lock(tmp_path, cancel_event=canceled):
            pass
    assert not (tmp_path / ".maintenance.lock").exists()


def test_cancellation_after_acquisition_keeps_lease_until_exit(tmp_path):
    canceled = threading.Event()
    with maintenance_lock(tmp_path, cancel_event=canceled):
        canceled.set()
        with pytest.raises(TimeoutError):
            with maintenance_lock(tmp_path, timeout=0.03):
                pytest.fail("holder cancellation silently released lease")
    with maintenance_lock(tmp_path):
        pass


def test_keyboard_interrupt_during_wait_releases_waiter_descriptor(tmp_path, monkeypatch):
    import bb_assistant.project_lock as project_lock

    def interrupted_sleep(delay):
        raise KeyboardInterrupt

    with maintenance_lock(tmp_path):
        with monkeypatch.context() as patch:
            patch.setattr(project_lock.time, "sleep", interrupted_sleep)
            with pytest.raises(KeyboardInterrupt):
                with maintenance_lock(tmp_path):
                    pass
    with maintenance_lock(tmp_path):
        pass


def test_interrupted_flock_is_retried(tmp_path, monkeypatch):
    import bb_assistant.project_lock as project_lock

    flock = project_lock.fcntl.flock
    interrupted = False

    def interrupt_once(fd, operation):
        nonlocal interrupted
        if operation & project_lock.fcntl.LOCK_NB and not interrupted:
            interrupted = True
            raise InterruptedError(errno.EINTR, "synthetic interruption")
        return flock(fd, operation)

    monkeypatch.setattr(project_lock.fcntl, "flock", interrupt_once)
    with maintenance_lock(tmp_path):
        assert interrupted


def test_flock_io_failure_is_sanitized_and_releases_descriptor(tmp_path, monkeypatch):
    import bb_assistant.project_lock as project_lock

    def failed_flock(fd, operation):
        raise OSError(errno.EIO, "synthetic unshareable failure detail")

    with monkeypatch.context() as patch:
        patch.setattr(project_lock.fcntl, "flock", failed_flock)
        with pytest.raises(project_lock.ProjectLockError, match="could not be safely acquired") as error:
            with maintenance_lock(tmp_path):
                pass
        assert "unshareable" not in str(error.value)
    with maintenance_lock(tmp_path):
        pass


@pytest.mark.parametrize("kind", ["file", "directory"])
def test_location_replacement_is_detected_after_acquisition(tmp_path, monkeypatch, kind):
    import bb_assistant.project_lock as project_lock

    directory = tmp_path / "project"
    directory.mkdir(mode=0o700)
    flock = project_lock.fcntl.flock

    def replace_after_lock(fd, operation):
        result = flock(fd, operation)
        if operation & project_lock.fcntl.LOCK_NB and not os.path.isdir(f"/proc/self/fd/{fd}"):
            if kind == "file":
                path = directory / ".maintenance.lock"
                path.rename(directory / "old.lock")
                path.touch(mode=0o600)
            else:
                directory.rename(tmp_path / "old-project")
                directory.mkdir(mode=0o700)
        return result

    with monkeypatch.context() as patch:
        patch.setattr(project_lock.fcntl, "flock", replace_after_lock)
        with pytest.raises(project_lock.ProjectLockError, match="changed while waiting"):
            with maintenance_lock(directory):
                pass
    with maintenance_lock(directory):
        pass


@pytest.mark.parametrize("timeout", [0, -1, 31, float("nan"), float("inf"), True])
def test_invalid_timeout_is_rejected(tmp_path, timeout):
    with pytest.raises(ValueError):
        with maintenance_lock(tmp_path, timeout=timeout):
            pass


@pytest.mark.parametrize("kind", ["relative", "unclean", "double slash", "missing", "public", "symlink", "ancestor symlink"])
def test_invalid_directories_are_rejected(tmp_path, kind):
    from bb_assistant.project_lock import ProjectLockError

    if kind == "relative":
        directory = "."
    elif kind == "unclean":
        directory = str(tmp_path) + "/."
    elif kind == "double slash":
        directory = "/" + str(tmp_path)
    elif kind == "missing":
        directory = tmp_path / "missing"
    elif kind == "public":
        tmp_path.chmod(0o755)
        directory = tmp_path
    else:
        link = tmp_path / "link"
        link.symlink_to(tmp_path, target_is_directory=True)
        directory = link
        if kind == "ancestor symlink":
            (tmp_path / "child").mkdir(mode=0o700)
            directory = link / "child"
    with pytest.raises(ProjectLockError):
        with maintenance_lock(directory):
            pass
    if kind == "public":
        assert tmp_path.stat().st_mode & 0o777 == 0o755
    assert not (tmp_path / "missing").exists()


@pytest.mark.parametrize("kind", ["public", "read-only", "symlink", "hardlink", "directory", "fifo"])
def test_invalid_lock_files_are_rejected(tmp_path, kind):
    from bb_assistant.project_lock import ProjectLockError

    path = tmp_path / ".maintenance.lock"
    if kind == "public":
        path.touch(mode=0o600)
        path.chmod(0o644)
    elif kind == "read-only":
        path.touch(mode=0o400)
    elif kind in {"symlink", "hardlink"}:
        other = tmp_path / "unrelated"
        other.write_bytes(b"untouched")
        other.chmod(0o600)
        if kind == "symlink":
            path.symlink_to(other)
        else:
            os.link(other, path)
    elif kind == "directory":
        path.mkdir(mode=0o700)
    else:
        os.mkfifo(path, mode=0o600)
    with pytest.raises(ProjectLockError):
        with maintenance_lock(tmp_path):
            pass
    if kind in {"symlink", "hardlink"}:
        assert other.read_bytes() == b"untouched"


def test_unsupported_platform_fails_explicitly(tmp_path, monkeypatch):
    import bb_assistant.project_lock as project_lock

    monkeypatch.setattr(project_lock.sys, "platform", "darwin")
    with pytest.raises(project_lock.ProjectLockError, match="Linux"):
        with maintenance_lock(tmp_path):
            pass


@pytest.mark.parametrize("filesystem", [None, "nfs", "cifs", "fuse", "unknown"])
def test_unknown_or_network_filesystem_fails_closed(tmp_path, monkeypatch, filesystem):
    import bb_assistant.project_lock as project_lock

    mount = None if filesystem is None else (filesystem, tmp_path)
    monkeypatch.setattr(project_lock, "_mount_for_path", lambda path: mount)
    with pytest.raises(project_lock.ProjectLockError, match="local filesystem"):
        with maintenance_lock(tmp_path):
            pass
