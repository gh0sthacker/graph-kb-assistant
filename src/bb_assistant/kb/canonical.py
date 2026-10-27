"""Stable, provenance-preserving KB document and chunk records."""
from __future__ import annotations

from dataclasses import dataclass
import hashlib
import json


CHUNK_SCHEMA = "redamon-compatible-chunk-v1"


def _digest(payload: object) -> str:
    encoded = json.dumps(payload, sort_keys=True, separators=(",", ":"), ensure_ascii=False).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


def redamon_chunk_id(source_name: str, unique_key: str) -> str:
    """Return RedAmon's stable 16-hex identifier for a chunk identity key."""
    return hashlib.sha256(f"{source_name}:{unique_key}".encode("utf-8")).hexdigest()[:16]


@dataclass(frozen=True, slots=True)
class SourceDocument:
    source_name: str
    source_id: str
    title: str
    text: str
    source_url: str | None
    source_locator: str | None
    source_revision: str | None
    license_id: str | None
    attribution: str | None
    metadata: dict[str, str | int | float | bool | None]
    content_sha256: str
    raw_source_text: str | None = None
    raw_source_sha256: str | None = None

    @classmethod
    def create(
        cls,
        *,
        source_name: str,
        source_id: str,
        title: str,
        text: str,
        source_url: str | None = None,
        source_locator: str | None = None,
        source_revision: str | None = None,
        license_id: str | None = None,
        attribution: str | None = None,
        metadata: dict[str, str | int | float | bool | None] | None = None,
        raw_source_text: str | None = None,
    ) -> "SourceDocument":
        return cls(
            source_name=source_name,
            source_id=source_id,
            title=title,
            text=text,
            source_url=source_url,
            source_locator=source_locator,
            source_revision=source_revision,
            license_id=license_id,
            attribution=attribution,
            metadata=metadata or {},
            content_sha256=hashlib.sha256(text.encode("utf-8")).hexdigest(),
            raw_source_text=raw_source_text,
            raw_source_sha256=(
                hashlib.sha256(raw_source_text.encode("utf-8")).hexdigest()
                if raw_source_text is not None else None
            ),
        )


@dataclass(frozen=True, slots=True)
class CanonicalChunk:
    chunk_id: str
    source_name: str
    source_id: str
    title: str
    ordinal: int
    start_char: int
    end_char: int
    text: str
    source_url: str | None
    source_locator: str | None
    source_revision: str | None
    license_id: str | None
    attribution: str | None
    metadata: dict[str, str | int | float | bool | None]
    source_sha256: str
    chunk_sha256: str
    chunk_schema: str = CHUNK_SCHEMA

    @classmethod
    def from_document(
        cls, document: SourceDocument, ordinal: int, start_char: int, end_char: int, text: str,
        *, title: str | None = None, metadata: dict[str, str | int | float | bool | None] | None = None,
        source_span_exact: bool = True, redamon_unique_key: str | None = None,
    ) -> "CanonicalChunk":
        if (ordinal < 0 or start_char < 0 or end_char < start_char
                or (source_span_exact and end_char - start_char != len(text)) or not text.strip()):
            raise ValueError("Chunk ordinal must be nonnegative and text must not be blank")
        chunk_hash = hashlib.sha256(text.encode("utf-8")).hexdigest()
        chunk_metadata = {**(document.metadata if metadata is None else metadata),
                          **({} if source_span_exact else {"source_span_exact": False})}
        identity = {
            "schema": CHUNK_SCHEMA,
            "source_name": document.source_name,
            "source_id": document.source_id,
            "source_sha256": document.content_sha256,
            "ordinal": ordinal,
            "start_char": start_char,
            "end_char": end_char,
            "chunk_sha256": chunk_hash,
        }
        chunk_id = _digest(identity)
        if redamon_unique_key is not None:
            if not redamon_unique_key:
                raise ValueError("RedAmon unique key must not be empty")
            chunk_metadata["redamon_unique_key"] = redamon_unique_key
            chunk_id = redamon_chunk_id(document.source_name, redamon_unique_key)
        return cls(
            chunk_id=chunk_id,
            source_name=document.source_name,
            source_id=document.source_id,
            title=title or document.title,
            ordinal=ordinal,
            start_char=start_char,
            end_char=end_char,
            text=text,
            source_url=document.source_url,
            source_locator=document.source_locator,
            source_revision=document.source_revision,
            license_id=document.license_id,
            attribution=document.attribution,
            metadata=chunk_metadata,
            source_sha256=document.content_sha256,
            chunk_sha256=chunk_hash,
        )


def chunk_document(document: SourceDocument, *, max_chars: int = 1920, overlap_chars: int = 0) -> tuple[CanonicalChunk, ...]:
    """Match RedAmon chunk content/titles and retain a truthful source span.

    RedAmon trims sections, normalizes merged paragraph separators and appends
    an ellipsis to clipped records. Non-literal chunks therefore carry a
    covering source span marked ``source_span_exact=False`` in metadata.
    """
    if max_chars < 256 or overlap_chars != 0:
        raise ValueError("RedAmon-compatible chunks require max_chars >= 256 and zero overlap")
    if not document.text or document.metadata.get("document_role") == "license_notice":
        return ()
    if document.source_name in {"gtfobins", "lolbas", "nuclei"}:
        rendered, excluded_reason = _redamon_yaml_chunks(document, max_chars=max_chars)
        return tuple(
            _canonical_redamon_chunk(
                document, i, title, text, key, truncated=truncated, fallback_reason=fallback,
                redamon_excluded_reason=excluded_reason,
            )
            for i, (title, text, key, truncated, fallback) in enumerate(rendered)
        )
    if document.source_name == "tool_docs" and document.metadata.get("category_dir") == "tooling":
        # RedAmon indexes each tooling file as one compact summary card and
        # keeps the tool name as its title; the summary's first line is not a
        # Markdown section heading.
        rendered = [(document.text, document.title, False)]
    else:
        rendered = (_markdown_chunks(document.text, max_chars)
                    if document.source_name in {"tool_docs", "owasp"}
                    else _structured_chunks(document, max_chars))
    chunks = []
    cursor = 0
    for content, title, truncated in rendered:
        start, end, exact = _source_span(document.text, content, cursor)
        metadata = {**document.metadata, "chunk_truncated": True} if truncated else document.metadata
        chunk_title = title
        if document.source_name == "tool_docs" and document.metadata.get("category_dir") != "tooling":
            tool_name = str(document.metadata.get("tool_name") or document.title)
            if tool_name.lower() not in title.lower():
                chunk_title = f"{tool_name} — {title}"
        if document.source_name == "owasp":
            test_id = _owasp_test_id(document)
            if test_id:
                chunk_title = f"OWASP {test_id}: {title}"
        unique_key = _redamon_key(document, title)
        chunks.append(CanonicalChunk.from_document(
            document, len(chunks), start, end, content, title=chunk_title, metadata=metadata,
            source_span_exact=exact, redamon_unique_key=unique_key,
        ))
        cursor = end
    return tuple(chunks)


def _canonical_redamon_chunk(
    document: SourceDocument, ordinal: int, title: str, text: str, unique_key: str, *,
    truncated: bool = False, fallback_reason: str | None = None,
    redamon_excluded_reason: str | None = None,
) -> CanonicalChunk:
    # These records are rendered from structured source fields, so they do not
    # claim a literal span in the original YAML/Markdown document.
    return CanonicalChunk.from_document(
        document, ordinal, 0, len(document.text), text, title=title,
        metadata={
            **document.metadata,
            **({"chunk_truncated": True} if truncated else {}),
            **({"canonicalization_fallback": fallback_reason} if fallback_reason else {}),
            **({"redamon_excluded_reason": redamon_excluded_reason} if redamon_excluded_reason else {}),
        },
        source_span_exact=False, redamon_unique_key=unique_key,
    )


def _redamon_key(document: SourceDocument, section_title: str) -> str:
    source = document.source_name
    metadata = document.metadata
    if source == "tool_docs":
        tool = str(metadata.get("tool_name") or document.title)
        section = "summary" if metadata.get("category_dir") == "tooling" else section_title.removeprefix(f"{tool} — ")
        return f"{tool}:{section}"
    if source == "owasp":
        test_id = _owasp_test_id(document)
        filename = str(metadata.get("filename") or document.source_id.rsplit("/", 1)[-1])
        return f"{test_id or filename}:{section_title}"
    if source in {"nvd", "exploitdb"}:
        value = metadata.get("cve_id") if source == "nvd" else metadata.get("edb_id")
        return str(value or document.source_id)
    if source == "nuclei":
        return str(metadata.get("template_id") or document.source_id.rsplit("/", 1)[-1].rsplit(".", 1)[0])
    return f"{document.source_id}:{section_title}"


def _owasp_test_id(document: SourceDocument) -> str:
    import re
    text = document.raw_source_text or document.text
    match = re.search(r"WSTG-[A-Z]+-\d+", text)
    return match.group(0) if match else ""


def _redamon_yaml_chunks(
    document: SourceDocument, *, max_chars: int,
) -> tuple[list[tuple[str, str, str, bool, str | None]], str | None]:
    """Render the RedAmon GTFOBins, LOLBAS, and Nuclei cards from pinned YAML.

    The input is third-party data: size and alias limits are checked before
    PyYAML's safe loader is used. Invalid records fail the source build closed.
    """
    import re
    import yaml

    raw = document.text
    if len(raw.encode("utf-8")) > 1024 * 1024:
        if document.source_name == "nuclei":
            return _nuclei_raw_fallback(document, max_chars, "yaml_size_limit"), None
        raise ValueError(f"{document.source_name} source document exceeds YAML size limit")
    max_indent = 0
    for line in raw.splitlines():
        stripped = line.lstrip(" ")
        if stripped and not stripped.startswith("#"):
            max_indent = max(max_indent, len(line) - len(stripped))
    # Count parsed YAML tokens, not punctuation in comments, quoted regexes,
    # or Nuclei payloads. The RedAmon char scan rejects valid templates with
    # thousands of literal wildcard characters; PyYAML's scanner preserves
    # the same hard alias/anchor/depth bounds without that false positive.
    from yaml.tokens import (
        AliasToken, AnchorToken, FlowMappingEndToken, FlowMappingStartToken,
        FlowSequenceEndToken, FlowSequenceStartToken,
    )
    max_flow_depth = flow_depth = anchors = aliases = 0
    try:
        for token in yaml.scan(raw, Loader=yaml.SafeLoader):
            if isinstance(token, (FlowMappingStartToken, FlowSequenceStartToken)):
                flow_depth += 1
                max_flow_depth = max(max_flow_depth, flow_depth)
            elif isinstance(token, (FlowMappingEndToken, FlowSequenceEndToken)):
                flow_depth = max(0, flow_depth - 1)
            elif isinstance(token, AnchorToken):
                anchors += 1
            elif isinstance(token, AliasToken):
                aliases += 1
    except yaml.YAMLError as exc:
        if document.source_name == "nuclei":
            return _nuclei_raw_fallback(document, max_chars, "yaml_scan_failed"), None
        raise ValueError(f"Invalid {document.source_name} YAML source document") from exc
    if max_indent > 256 or max_flow_depth > 64 or anchors > 100 or aliases > 1000:
        if document.source_name == "nuclei":
            return _nuclei_raw_fallback(document, max_chars, "yaml_complexity_limit"), None
        raise ValueError(f"{document.source_name} source document exceeds YAML complexity limits")
    try:
        if document.source_name == "gtfobins":
            match = re.match(r"^---\s*\n(.*?)\n(?:\.\.\.|---)(?:\s|$)", raw, re.DOTALL)
            value = yaml.safe_load(match.group(1) if match else raw.strip().removeprefix("---").strip())
            binary = str(document.metadata.get("binary_name") or document.title)
            functions = value.get("functions", {}) if isinstance(value, dict) else {}
            output: list[tuple[str, str, str, bool, str | None]] = []
            if not isinstance(functions, dict):
                raise ValueError("GTFOBins functions is not a mapping")
            for function_type, rows in functions.items():
                if rows is None:
                    continue
                for row in rows if isinstance(rows, list) else [rows]:
                    if not isinstance(row, dict):
                        continue
                    contexts = row.get("contexts") or {}
                    if isinstance(contexts, dict):
                        contexts = [str(k) for k, v in contexts.items() if v is not False]
                    elif isinstance(contexts, list):
                        contexts = [str(v) for v in contexts if v]
                    else:
                        contexts = []
                    description = row.get("description", "")
                    code = row.get("code", "")
                    comment = row.get("comment", "") or ""
                    if isinstance(comment, str):
                        comment = comment.strip()
                    text = f"{binary} — {function_type}"
                    if description:
                        text += f": {description}"
                    if contexts:
                        text += f"\nContexts: {', '.join(contexts)}"
                    if code:
                        text += f"\nCommand: {code}"
                    if comment:
                        text += f"\nNote: {comment}"
                    output.append((f"{binary} — {function_type}", text, f"{binary}:{function_type}", False, None))
            return output, None

        value = yaml.safe_load(raw)
        if document.source_name == "nuclei":
            if not isinstance(value, dict):
                raise ValueError("Nuclei YAML is not a mapping")
            template_id = (
                value.get("id") or value.get("template-id") or value.get("ID")
                or document.metadata.get("template_id")
                or document.source_id.rsplit("/", 1)[-1].rsplit(".", 1)[0]
            )
            if not template_id:
                raise ValueError("Nuclei template has no ID")
            info = value.get("info", value)
            info = info if isinstance(info, dict) else {}
            name = info.get("name", "") or str(template_id)
            severity = info.get("severity", "info")
            description = _as_text(info.get("description"))
            variables_raw = value.get("variables")
            variables = {str(k): str(v) for k, v in variables_raw.items()} if isinstance(variables_raw, dict) else {}
            flow = _as_text(value.get("flow"))
            content = f"{name} ({severity})" if name else f"{template_id} ({severity})"
            if description:
                content += f"\n\n{description}"
            if variables:
                content += "\n\nVariables: " + ", ".join(f"{k}={v}" for k, v in variables.items())
            if flow:
                content += f"\n\nFlow:\n{flow}"
            http = _nuclei_http(value.get("http"))
            dns = _nuclei_dns(value.get("dns"))
            code = _nuclei_code(value.get("code"))
            tcp = _nuclei_tcp(value.get("tcp") or value.get("network"))
            javascript = _nuclei_javascript(value.get("javascript"))
            for i, block in enumerate(http, start=1):
                label = f"HTTP step {i}" if len(http) > 1 else "HTTP"
                if block["raw"]:
                    for request in block["raw"]:
                        content += f"\n\n{label} (raw):\n{request}"
                else:
                    line = f"\n\n{label}:"
                    if block["method"] and block["paths"]:
                        line += f" {block['method']} {'; '.join(block['paths'])}"
                    elif block["method"]:
                        line += f" {block['method']}"
                    elif block["paths"]:
                        line += f" {'; '.join(block['paths'])}"
                    content += line
                    if block["body"]:
                        content += f"\nBody: {block['body']}"
                if block["payloads"]:
                    content += f"\nPayloads: {block['payloads']}"
            for i, block in enumerate(dns, start=1):
                label = f"DNS query {i}" if len(dns) > 1 else "DNS query"
                line = f"\n\n{label}: {block['name']}"
                if block["type"]:
                    line += f" ({block['type']}" + (f"/{block['class']}" if block["class"] else "") + ")"
                content += line
            for i, block in enumerate(code, start=1):
                if not block["source"].strip():
                    continue
                label = f"Code step {i}" if len(code) > 1 else "Code"
                if block["engine"]:
                    label += f" ({block['engine']})"
                content += f"\n\n{label}:\n{block['source'].strip()}"
            for i, block in enumerate(tcp, start=1):
                label = f"TCP probe {i}" if len(tcp) > 1 else "TCP probe"
                if block["port"]:
                    label += f" (port {block['port']})"
                content += f"\n\n{label}" + (f": {'; '.join(block['inputs'])}" if block["inputs"] else "")
            for i, block in enumerate(javascript, start=1):
                if not block["code"].strip():
                    continue
                label = f"JavaScript step {i}" if len(javascript) > 1 else "JavaScript"
                content += f"\n\n{label}:\n{block['code'].strip()}"
                if block["payloads"]:
                    content += f"\nPayloads: {block['payloads']}"
            title = f"{template_id} — {name}" if name != str(template_id) else str(template_id)
            return [(title, content, str(template_id), False, None)], _redamon_nuclei_exclusion(raw, value)

        if not isinstance(value, dict) or "Name" not in value:
            raise ValueError("LOLBAS YAML has no Name")
        name = str(value.get("Name") or "unknown")
        binary_description = value.get("Description", "") or ""
        paths: list[str] = []
        for path in value.get("Full_Path") or []:
            if isinstance(path, dict) and isinstance(path.get("Path"), str) and path["Path"]:
                paths.append(path["Path"])
            elif isinstance(path, str):
                paths.append(path)
        output = []
        for command_row in value.get("Commands", []):
            if not isinstance(command_row, dict):
                continue
            command = command_row.get("Command", "") or ""
            category = command_row.get("Category", "") or ""
            title = f"{name} — {category}"
            parts = [title]
            command_description = command_row.get("Description", "") or ""
            if binary_description:
                parts.extend(("", str(binary_description)))
            if command_description:
                parts.extend(("", str(command_description)))
            if command:
                parts.extend(("", f"Command: {command}"))
            for field_name, label in (("Usecase", "Usecase"), ("Privileges", "Privileges"), ("OperatingSystem", "OS")):
                field = command_row.get(field_name, "") or ""
                if field:
                    parts.append(f"{label}: {field}")
            tags: list[str] = []
            for tag in command_row.get("Tags") or []:
                if isinstance(tag, dict):
                    tags.extend(f"{key}: {item}" for key, item in tag.items())
                elif isinstance(tag, str):
                    tags.append(tag)
            if tags:
                parts.append(f"Tags: {', '.join(tags)}")
            mitre = command_row.get("MitreID", "") or ""
            if mitre:
                parts.append(f"MITRE: {mitre}")
            if paths:
                parts.append(f"Full Path: {'; '.join(paths)}")
            unique = f"{name}:{category}:{str(command)[:50]}"
            content = "\n".join(parts)
            output.append((title, content, unique, False, None))
        return output, None
    except (yaml.YAMLError, UnicodeError) as exc:
        if document.source_name == "nuclei":
            return _nuclei_raw_fallback(document, max_chars, "yaml_parse_failed"), None
        raise ValueError(f"Invalid {document.source_name} YAML source document") from exc


def _redamon_nuclei_exclusion(raw: str, value: dict) -> str | None:
    """Retain valid extra templates while matching RedAmon's default corpus.

    Pinned RedAmon file_cache._scan_yaml_complexity counts literal punctuation
    even in strings/comments. Our token-based parser accepts those valid YAML
    files, so label the resulting cards supplemental instead of discarding
    them or silently adding them to the strict reference candidate pool.
    """
    if not value.get("id"):
        return "missing_top_level_id"
    if raw.count("&") > 100:
        return "yaml_anchor_character_limit"
    if raw.count("*") > 1000:
        return "yaml_alias_character_limit"
    depth = 0
    for char in raw:
        if char in "{[":
            depth += 1
            if depth > 64:
                return "yaml_flow_character_limit"
        elif char in "}]" and depth:
            depth -= 1
    return None


def _nuclei_raw_fallback(
    document: SourceDocument, max_chars: int, reason: str,
) -> list[tuple[str, str, str, bool, str | None]]:
    """Keep every bounded Nuclei source searchable when structure is unusable."""
    template_id = str(
        document.metadata.get("template_id")
        or document.source_id.rsplit("/", 1)[-1].rsplit(".", 1)[0]
    )
    remaining = document.text
    parts: list[str] = []
    while remaining:
        if len(remaining) <= max_chars:
            parts.append(remaining)
            break
        boundary = remaining.rfind("\n", 0, max_chars + 1)
        if boundary <= 0:
            boundary = remaining.rfind(" ", 0, max_chars + 1)
        if boundary <= 0:
            boundary = max_chars
        parts.append(remaining[:boundary])
        remaining = remaining[boundary:]
    if not parts:
        return []
    total = len(parts)
    return [
        (
            f"{template_id} — Raw template" + (f" (part {i + 1})" if total > 1 else ""),
            text,
            template_id if i == 0 else f"{template_id}:raw:{i + 1}",
            False,
            reason,
        )
        for i, text in enumerate(parts)
    ]


def _as_text(value: object) -> str:
    if value is None:
        return ""
    return value.strip() if isinstance(value, str) else str(value).strip()


def _payload_text(value: object) -> str:
    if not isinstance(value, dict):
        return ""
    return "; ".join(f"{key}={','.join(map(str, item)) if isinstance(item, list) else item}" for key, item in value.items())


def _nuclei_http(raw: object) -> list[dict[str, object]]:
    if not isinstance(raw, list):
        return []
    output = []
    for row in raw:
        if not isinstance(row, dict):
            continue
        method = row.get("method")
        method = method.upper() if isinstance(method, str) else ""
        paths = row.get("path")
        paths = [str(x) for x in paths if x] if isinstance(paths, list) else ([paths] if isinstance(paths, str) and paths else [])
        body = row.get("body")
        body = body.strip() if isinstance(body, str) else ""
        raw_requests = row.get("raw")
        raw_requests = [x.strip() for x in raw_requests if isinstance(x, str) and x.strip()] if isinstance(raw_requests, list) else []
        payloads = _payload_text(row.get("payloads"))
        if method or paths or body or raw_requests:
            output.append({"method": method, "paths": paths, "body": body, "raw": raw_requests, "payloads": payloads})
    return output


def _nuclei_dns(raw: object) -> list[dict[str, str]]:
    if not isinstance(raw, list):
        return []
    return [{"name": row["name"], "type": row.get("type", "").upper() if isinstance(row.get("type"), str) else "",
             "class": row.get("class", "") if isinstance(row.get("class"), str) else ""}
            for row in raw if isinstance(row, dict) and isinstance(row.get("name"), str) and row["name"]]


def _nuclei_code(raw: object) -> list[dict[str, str]]:
    if not isinstance(raw, list):
        return []
    output = []
    for row in raw:
        if not isinstance(row, dict) or not isinstance(row.get("source"), str):
            continue
        engine = row.get("engine")
        engine = ",".join(map(str, engine)) if isinstance(engine, list) else engine if isinstance(engine, str) else ""
        output.append({"engine": engine, "source": row["source"]})
    return output


def _nuclei_tcp(raw: object) -> list[dict[str, object]]:
    if not isinstance(raw, list):
        return []
    output = []
    for row in raw:
        if not isinstance(row, dict):
            continue
        port = row.get("port")
        port = str(port[0]) if isinstance(port, list) and port else str(port) if port is not None and not isinstance(port, list) else ""
        inputs = [entry["data"] for entry in row.get("inputs", []) if isinstance(entry, dict) and isinstance(entry.get("data"), str) and entry["data"]] if isinstance(row.get("inputs"), list) else []
        output.append({"port": port, "inputs": inputs})
    return output


def _nuclei_javascript(raw: object) -> list[dict[str, str]]:
    if not isinstance(raw, list):
        return []
    return [{"code": row["code"], "payloads": _payload_text(row.get("payloads"))}
            for row in raw if isinstance(row, dict) and isinstance(row.get("code"), str)]


def _estimated_tokens(text: str) -> int:
    return len(text) // 4


def _structured_chunks(document: SourceDocument, limit: int) -> list[tuple[str, str, bool]]:
    # RedAmon stores each structured record whole. Its embedder tokenizer may
    # truncate model input, but that must not truncate the searchable source
    # text persisted alongside the vector.
    return [(document.text, document.title, False)]


def _markdown_chunks(text: str, limit: int) -> list[tuple[str, str, bool]]:
    import re

    sections = [section.strip() for section in re.split(r"(?m)(?=^##\s)", text) if section.strip()]
    parsed = []
    for section in sections:
        lines = section.split("\n", 1)
        parsed.append((lines[0].lstrip("#").strip(), section.strip()))

    merged: list[tuple[str, str]] = []
    pending: tuple[str, str] | None = None
    for title, content in parsed:
        if pending is not None:
            combined = pending[1] + "\n\n" + content
            if _estimated_tokens(combined) <= limit // 4:
                pending = (pending[0], combined)
                continue
            merged.append(pending)
            pending = None
        if _estimated_tokens(content) < 128:
            pending = (title, content)
        else:
            merged.append((title, content))
    if pending is not None:
        merged.append(pending)

    result: list[tuple[str, str, bool]] = []
    for title, content in merged:
        if _estimated_tokens(content) <= limit // 4:
            result.append((content, title, False))
            continue
        paragraphs = re.split(r"\n\n+", content)
        current = ""
        parts: list[str] = []
        for paragraph in paragraphs:
            candidate = (current + "\n\n" + paragraph).strip() if current else paragraph
            if _estimated_tokens(candidate) > limit // 4 and current:
                parts.append(current.strip())
                current = paragraph
            else:
                current = candidate
        if current.strip():
            parts.append(current.strip())
        for part, chunk_text in enumerate(parts, start=1):
            part_title = f"{title} (part {part})" if part > 1 or len(paragraphs) > 1 else title
            result.append((chunk_text, part_title, False))
    return result


def _source_span(source: str, chunk: str, cursor: int) -> tuple[int, int, bool]:
    exact_start = source.find(chunk, cursor)
    if exact_start >= 0:
        return exact_start, exact_start + len(chunk), True
    searchable = chunk[:-3] if chunk.endswith("...") else chunk
    literal_start = source.find(searchable, cursor)
    if literal_start >= 0:
        return literal_start, literal_start + len(searchable), False
    prefix = searchable[:min(80, len(searchable))].rstrip()
    suffix = searchable[-min(80, len(searchable)):].lstrip()
    start = source.find(prefix, cursor) if prefix else -1
    if start < 0:
        first_line = next((line.strip() for line in chunk.splitlines() if line.strip()), "")
        prefix = first_line[:min(80, len(first_line))]
        start = source.find(prefix, cursor) if prefix else -1
    if start < 0:
        raise ValueError("Cannot map RedAmon-compatible chunk to source text")
    suffix_start = source.find(suffix, start + len(prefix), min(len(source), start + len(chunk) + 4096)) if suffix else -1
    end = suffix_start + len(suffix) if suffix_start >= start else min(len(source), start + len(searchable))
    return start, end, source[start:end] == chunk
