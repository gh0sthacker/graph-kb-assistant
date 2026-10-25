"""Provider contract and canonical adapter for RedAmon's seven KB feeds.

The RedAmon curation clients import their own ``knowledge_base`` package and
optional runtime dependencies. This module does not import them, mutate
``sys.path``, access the network, read credentials, or write source caches.
Instantiate or wrap a client in its supported runtime and inject a provider
that returns :class:`SourceBatch`; file records must include original UTF-8
text, exact locators, revisions, and source metadata. API records should
retain the individual original JSON object as deterministic JSON text.

The current NVD manifest selects the RedAmon ``full`` profile, a 730-day
publication window, and all severities. The default refresh path can use
different standard/default settings. Providers report effective filters, and
the adapter marks mismatches partial instead of substituting client defaults.
Providers wrapping RedAmon's archive clients must request full records (the
clients otherwise commonly return changed files only), and the NVD provider
must pass the manifest's explicit full-profile arguments.

Only source families whose manifest status establishes eligible terms reach a
provider. Unresolved families remain visible as blocked coverage with source
terms, evidence, and a resolution path. ExploitDB content is always rebuilt
from an allowlist of descriptions and metadata; raw rows and PoC/source-code
fields are never copied.
"""
from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime, timedelta, timezone
import json
import re
from typing import Callable, Mapping, Sequence

from .canonical import CanonicalChunk, SourceDocument, chunk_document


SOURCE_FAMILIES = (
    "tool_docs", "gtfobins", "lolbas", "owasp", "exploitdb", "nvd", "nuclei"
)
_ELIGIBLE = frozenset({
    "confirmed_at_pinned_revision",
    "public_domain_confirmed",
    "terms_confirmed_with_required_notices",
})
_SHA256 = re.compile(r"^[0-9a-f]{64}$")
_COMMIT = re.compile(r"^[0-9a-f]{40}$")
_EXPLOIT_FIELDS = frozenset({
    "id", "description", "date_published", "platform", "type", "port", "codes"
})
_EXPLOIT_CODE_KEYS = ("code", "poc", "payload", "source", "exploit")
_RESOLUTION = {
    "tool_docs": "Preserve the MIT license notice from the pinned RedAmon source snapshot.",
    "gtfobins": "Retain the pinned revision's LICENSE/NOTICE text and required attribution.",
    "lolbas": "Verify license and notice terms at the pinned commit, then retain that evidence.",
    "owasp": "Verify WSTG terms at the pinned commit and retain its license notice and attribution.",
    "exploitdb": "Confirm terms for the CSV description/metadata subset independently of the repository license; exclude PoC source.",
    "nvd": "Complete NIST attribution and review publisher-contributed, CVE affected, and other embedded content at the selected source boundary.",
    "nuclei": "Verify template terms and required notices at the pinned commit.",
}


@dataclass(frozen=True, slots=True)
class SourceRequest:
    source_name: str
    profile: str
    source_spec: Mapping[str, object]
    expected_revision: str | None
    filters: Mapping[str, object]


@dataclass(frozen=True, slots=True)
class SourceRecord:
    """One original feed document or a license/notice document."""

    source_id: str
    title: str
    original_text: str
    source_locator: str | None
    source_url: str | None = None
    source_revision: str | None = None
    metadata: Mapping[str, object] = field(default_factory=dict)
    document_role: str = "content"
    search_text: str | None = None


@dataclass(frozen=True, slots=True)
class SourceBatch:
    """Exact provider result; counts cover content records, not extra notices.

    ``expected_count`` is an exact upstream total or local enumeration count,
    never an estimate. ``applied_filters`` contains effective, non-secret
    settings. ``notices_complete`` is true only if all applicable license and
    notice documents are included as ``license_notice`` records.
    """

    source_revision: str
    records: Sequence[SourceRecord]
    expected_count: int | None
    fetched_count: int | None
    parsed_count: int | None
    rejected_count: int | None = 0
    complete: bool = False
    artifact_sha256: str | None = None
    fetched_at: str | None = None
    response_sha256: str | None = None
    applied_filters: Mapping[str, object] = field(default_factory=dict)
    notices_complete: bool = False
    limitations: tuple[str, ...] = ()


@dataclass(frozen=True, slots=True)
class RedAmonSnapshot:
    source_manifest_sha256: str
    coverage_state: str
    documents: tuple[SourceDocument, ...]
    chunks: tuple[CanonicalChunk, ...]
    source_coverage: tuple[Mapping[str, object], ...]
    manifest_blockers: tuple[str, ...] = ()

    def coverage_report(self) -> dict[str, object]:
        return {
            "schema_version": 1,
            "source_manifest_sha256": self.source_manifest_sha256,
            "coverage_state": self.coverage_state,
            "source_count": len(self.source_coverage),
            "document_count": len(self.documents),
            "chunk_count": len(self.chunks),
            "blocked_source_count": sum(row["state"] == "blocked" for row in self.source_coverage),
            "partial_source_count": sum(row["state"] == "partial" for row in self.source_coverage),
            "manifest_blockers": list(self.manifest_blockers),
            "sources": [dict(row) for row in self.source_coverage],
        }


Provider = Callable[[SourceRequest], SourceBatch]


class RedAmonSourceAdapter:
    """Build deterministic canonical records from injected source providers.

    Pass the parsed ``sources.yaml`` mapping and SHA-256 of its exact file
    bytes. Providers can wrap RedAmon's source clients, but must return the
    original documents paired with parsed metadata. No YAML parser or RedAmon
    dependency is required here.
    """

    def __init__(
        self,
        manifest: Mapping[str, object],
        providers: Mapping[str, Provider],
        *,
        source_manifest_sha256: str,
    ) -> None:
        if not _SHA256.fullmatch(source_manifest_sha256):
            raise ValueError("source_manifest_sha256 must be a lowercase SHA-256 digest")
        self.manifest = manifest
        self.providers = providers
        self.manifest_sha256 = source_manifest_sha256

    def build(self, *, max_chars: int = 1920, overlap_chars: int = 0) -> RedAmonSnapshot:
        specs, manifest_blockers = self._source_specs()
        profile = str(self.manifest.get("profile") or "")
        documents: list[SourceDocument] = []
        chunks: list[CanonicalChunk] = []
        coverage: list[Mapping[str, object]] = []

        for family in SOURCE_FAMILIES:
            spec = specs.get(family)
            if spec is None:
                coverage.append(self._coverage(
                    family, "partial", blockers=("source_missing_from_manifest",)
                ))
                continue

            filters = self._filters(family, spec)
            details = self._source_details(spec)
            if family == "nvd" and filters.get("profile") != profile:
                coverage.append(self._coverage(
                    family, "blocked", blockers=("nvd_filter_profile_does_not_match_manifest_profile",),
                    requested_filters=filters, source_metadata=details,
                ))
                continue
            rights_blockers = self._rights_blockers(family, spec)
            if rights_blockers:
                coverage.append(self._coverage(
                    family, "blocked", blockers=rights_blockers,
                    requested_filters=filters, source_metadata=details,
                ))
                continue

            provider = self.providers.get(family)
            if provider is None:
                coverage.append(self._coverage(
                    family, "partial", blockers=("eligible_source_provider_not_configured",),
                    requested_filters=filters, source_metadata=details,
                ))
                continue

            request = SourceRequest(
                family, profile, spec, _string(spec.get("revision")), filters
            )
            try:
                batch = provider(request)
            except Exception as exc:  # suppress provider messages that may contain secrets
                safe_code = getattr(exc, "coverage_code", None)
                if (
                    family != "nvd"
                    or type(exc).__module__ != "bb_assistant.kb.nvd"
                    or type(exc).__name__ != "NVDProviderError"
                    or not isinstance(safe_code, str)
                    or not safe_code.startswith(f"{family}_")
                    or not re.fullmatch(r"[a-z][a-z0-9_]{0,79}", safe_code)
                ):
                    safe_code = type(exc).__name__
                coverage.append(self._coverage(
                    family, "partial", blockers=(f"provider_failed:{safe_code}",),
                    requested_filters=filters, source_metadata=details,
                    fetched_count=None, parsed_count=None, rejected_count=None,
                ))
                continue
            if not isinstance(batch, SourceBatch):
                coverage.append(self._coverage(
                    family, "blocked", blockers=("provider_must_return_SourceBatch",),
                    requested_filters=filters, source_metadata=details,
                ))
                continue

            revision, blocker = self._revision(family, spec, batch)
            if blocker:
                coverage.append(self._coverage(
                    family, "blocked", blockers=(blocker,), requested_filters=filters,
                    applied_filters=batch.applied_filters, source_metadata=details,
                    source_revision=batch.source_revision or None,
                    expected_count=_count(batch.expected_count),
                    fetched_count=_count(batch.fetched_count),
                    parsed_count=_count(batch.parsed_count),
                    rejected_count=_count(batch.rejected_count),
                    artifact_sha256=batch.artifact_sha256,
                    limitations=batch.limitations,
                ))
                continue

            docs, record_rejections, record_blockers = self._documents(
                family, spec, batch, revision, profile
            )
            docs.sort(key=lambda item: item.source_id)
            family_chunks = [
                chunk
                for document in docs
                for chunk in chunk_document(document, max_chars=max_chars, overlap_chars=overlap_chars)
            ]
            documents.extend(docs)
            chunks.extend(family_chunks)

            expected, fetched = _count(batch.expected_count), _count(batch.fetched_count)
            parsed, provider_rejected = _count(batch.parsed_count), _count(batch.rejected_count)
            blockers = list(record_blockers)
            content_records = sum(
                isinstance(record, SourceRecord) and record.document_role == "content"
                for record in batch.records
            )
            blockers.extend(self._count_blockers(
                batch, expected, fetched, parsed, provider_rejected, content_records
            ))
            mismatches = [key for key, value in filters.items() if batch.applied_filters.get(key) != value]
            if mismatches:
                blockers.append("applied_filters_do_not_match_manifest:" + ",".join(sorted(mismatches)))
            if not batch.complete:
                blockers.append("provider_did_not_confirm_complete_fetch")
            has_notice = any(
                isinstance(record, SourceRecord) and record.document_role == "license_notice"
                for record in batch.records
            )
            notice_required = spec.get("kind") in {"github_archive", "local_markdown"}
            if not batch.notices_complete or (notice_required and not has_notice):
                blockers.append("source_license_or_notice_documents_incomplete")
            if batch.artifact_sha256 is not None and not _SHA256.fullmatch(batch.artifact_sha256):
                blockers.append("invalid_artifact_sha256")
            blockers = list(dict.fromkeys(blockers))

            coverage.append(self._coverage(
                family, "partial" if blockers else "done", blockers=tuple(blockers),
                requested_filters=filters, applied_filters=batch.applied_filters,
                source_metadata=details, source_revision=revision,
                expected_count=expected, fetched_count=fetched, parsed_count=parsed,
                rejected_count=None if provider_rejected is None else provider_rejected + record_rejections,
                document_count=len(docs), chunk_count=len(family_chunks),
                license_notice_count=sum(doc.metadata.get("document_role") == "license_notice" for doc in docs),
                artifact_sha256=batch.artifact_sha256, limitations=batch.limitations,
            ))

        documents.sort(key=lambda item: (item.source_name, item.source_id))
        chunks.sort(key=lambda item: (SOURCE_FAMILIES.index(item.source_name), item.source_id, item.ordinal))
        states = [str(row["state"]) for row in coverage]
        if len(coverage) == len(SOURCE_FAMILIES) and all(state == "done" for state in states) and not manifest_blockers:
            state = "DONE"
        elif not manifest_blockers and not documents and states and all(value == "blocked" for value in states):
            state = "BLOCKED"
        else:
            state = "PARTIAL"
        return RedAmonSnapshot(
            self.manifest_sha256, state, tuple(documents), tuple(chunks),
            tuple(coverage), manifest_blockers,
        )

    def _source_specs(self) -> tuple[dict[str, Mapping[str, object]], tuple[str, ...]]:
        raw = self.manifest.get("sources")
        if not isinstance(raw, Sequence) or isinstance(raw, (str, bytes)):
            return {}, ("manifest_sources_not_a_sequence",)
        result: dict[str, Mapping[str, object]] = {}
        blockers: list[str] = []
        if self.manifest.get("profile") != "full":
            blockers.append("manifest_profile_must_be_full_for_seven_family_snapshot")
        for item in raw:
            if not isinstance(item, Mapping):
                blockers.append("manifest_source_entry_not_a_mapping")
                continue
            name = _string(item.get("id"))
            if name not in SOURCE_FAMILIES:
                blockers.append(f"unknown_manifest_source:{name or 'empty'}")
            elif name in result:
                blockers.append(f"duplicate_manifest_source:{name}")
            else:
                result[name] = item
        return result, tuple(blockers)

    @staticmethod
    def _rights_blockers(family: str, spec: Mapping[str, object]) -> tuple[str, ...]:
        status = _string(spec.get("license_status"))
        license_id = _string(spec.get("license_id")) or _string(spec.get("repository_license_id"))
        blockers = []
        if status not in _ELIGIBLE:
            if status == "current_repo_confirmed_pinned_revision_pending":
                blockers.append("license_confirmation_at_exact_pinned_revision_pending")
            elif status == "attribution_and_embedded-content-review":
                blockers.append("nvd_attribution_and_embedded_third_party_review_pending")
            elif status == "review_required":
                blockers.append("source_data_terms_review_required")
            else:
                blockers.append(f"license_status_unknown_or_ineligible:{status or 'unknown'}")
        if not license_id:
            blockers.append("license_id_missing")
        if not _string(spec.get("license_evidence")):
            blockers.append("license_evidence_missing")
        if family == "exploitdb" and spec.get("dataset_terms") not in _ELIGIBLE:
            blockers.append(f"exploitdb_dataset_terms_unresolved:{_string(spec.get('dataset_terms')) or 'unknown'}")
        if family == "nvd" and status != "terms_confirmed_with_required_notices":
            blockers.append("nvd_requires_composite_terms_and_notice_status")
        if family == "nvd" and spec.get("embedded_content_review") != "complete":
            blockers.append("nvd_embedded_third_party_content_review_not_complete")
        if family == "nvd" and status == "terms_confirmed_with_required_notices":
            if not _string(spec.get("cve_terms_evidence")):
                blockers.append("nvd_cve_terms_evidence_missing")
            if not _string(spec.get("cve_terms_reviewed_at")):
                blockers.append("nvd_cve_terms_review_date_missing")
            if not _string(spec.get("cve_copyright_designation")):
                blockers.append("nvd_cve_copyright_designation_missing")
            if not _string(spec.get("nvd_citation")):
                blockers.append("nvd_dataset_citation_missing")
        return tuple(blockers)

    @staticmethod
    def _filters(family: str, spec: Mapping[str, object]) -> dict[str, object]:
        if family == "tool_docs":
            return {
                "include_categories": tuple(spec.get("include_categories", ())),
                "exclude": tuple(spec.get("exclude", ())),
            }
        if family == "nvd":
            value = spec.get("filter")
            return dict(value) if isinstance(value, Mapping) else {}
        if family == "exploitdb":
            return {"content_scope": spec.get("content_scope")}
        # These are the filters implemented by RedAmon's existing clients.
        defaults = {
            "gtfobins": {"source_directory": "_gtfobins"},
            "lolbas": {"source_directory": "yml"},
            "owasp": {"test_directory": "document/4-Web_Application_Security_Testing", "exclude_readme": True},
            "nuclei": {
                "extensions": (".yaml", ".yml"),
                "exclude_prefixes": (".github/",),
                "exclude_paths": (".pre-commit-config.yaml", ".pre-commit-config.yml"),
            },
        }
        return defaults.get(family, {})

    @staticmethod
    def _source_details(spec: Mapping[str, object]) -> dict[str, object]:
        keys = (
            "id", "kind", "url", "revision", "revision_date", "revision_source", "license_id",
            "repository_license_id", "license_status", "license_evidence",
            "dataset_terms", "content_scope", "attribution", "embedded_content_review", "filter",
            "local_root", "include_categories", "exclude",
        )
        return {key: spec[key] for key in keys if key in spec}

    @staticmethod
    def _revision(family: str, spec: Mapping[str, object], batch: SourceBatch) -> tuple[str | None, str | None]:
        pinned = _string(spec.get("revision"))
        if pinned:
            return (pinned, None) if batch.source_revision == pinned else (None, "provider_revision_does_not_match_manifest_pin")
        source = _string(spec.get("revision_source"))
        if source == "redamon_git_commit":
            return (batch.source_revision, None) if _COMMIT.fullmatch(batch.source_revision or "") else (None, "tool_docs_requires_full_redamon_git_commit")
        if source == "fetch_timestamp_and_response_hashes":
            if not batch.fetched_at or not batch.response_sha256 or not _SHA256.fullmatch(batch.response_sha256):
                return None, "nvd_requires_fetch_timestamp_and_response_sha256"
            try:
                at = datetime.fromisoformat(batch.fetched_at.replace("Z", "+00:00"))
            except ValueError:
                return None, "nvd_fetch_timestamp_is_invalid"
            if at.tzinfo is None:
                return None, "nvd_fetch_timestamp_must_include_timezone"
            revision = f"{at.isoformat()};response_sha256={batch.response_sha256}"
            return (revision, None) if batch.source_revision == revision else (None, "nvd_revision_must_bind_fetch_timestamp_and_response_hash")
        return (batch.source_revision, None) if batch.source_revision else (None, "source_revision_missing")

    def _documents(
        self, family: str, spec: Mapping[str, object], batch: SourceBatch,
        revision: str, profile: str,
    ) -> tuple[list[SourceDocument], int, list[str]]:
        docs: list[SourceDocument] = []
        rejected = 0
        blockers: list[str] = []
        seen: set[str] = set()
        for record in batch.records:
            if not isinstance(record, SourceRecord):
                rejected += 1
                blockers.append("provider_record_not_a_SourceRecord")
                continue
            if not record.source_id or record.source_id in seen:
                rejected += 1
                blockers.append("missing_or_duplicate_record_id")
                continue
            seen.add(record.source_id)
            if not record.source_locator:
                rejected += 1
                blockers.append("source_locator_missing")
                continue
            if record.source_revision and record.source_revision != revision:
                rejected += 1
                blockers.append("record_revision_differs_from_batch_revision")
                continue
            if record.document_role not in {"content", "license_notice"}:
                rejected += 1
                blockers.append("unsupported_document_role")
                continue
            if record.document_role == "content" and family == "tool_docs" and not self._tool_doc_allowed(record, spec):
                rejected += 1
                blockers.append("tool_doc_outside_manifest_include_exclude_filter")
                continue
            if record.document_role == "content" and family == "nvd" and not self._nvd_allowed(record, spec, batch):
                rejected += 1
                blockers.append("nvd_record_outside_or_missing_manifest_filter_fields")
                continue

            metadata: Mapping[str, object] = record.metadata
            if record.document_role == "license_notice":
                text = record.original_text
            elif family == "exploitdb":
                text, metadata, error = self._exploitdb_record(record)
                if error:
                    rejected += 1
                    blockers.append(error)
                    continue
            else:
                text = record.original_text
            if record.search_text is not None:
                text = record.search_text
            if not text.strip():
                rejected += 1
                blockers.append("original_source_document_missing_or_blank")
                continue

            docs.append(SourceDocument.create(
                source_name=family,
                source_id=record.source_id,
                title=record.title or record.source_id,
                text=text,
                source_url=record.source_url or _string(spec.get("url")),
                source_locator=record.source_locator,
                source_revision=revision,
                license_id=_string(spec.get("license_id")) or _string(spec.get("repository_license_id")),
                attribution=_string(record.metadata.get("attribution")) or _string(spec.get("attribution")),
                metadata=self._metadata(metadata, family, spec, batch, profile, record.document_role),
                raw_source_text=record.original_text if record.search_text is not None else None,
            ))

        if spec.get("kind") in {"github_archive", "local_markdown"} and not any(
            isinstance(record, SourceRecord) and record.document_role == "license_notice"
            for record in batch.records
        ):
            blockers.append("license_notice_document_missing")
        return docs, rejected, list(dict.fromkeys(blockers))

    @staticmethod
    def _tool_doc_allowed(record: SourceRecord, spec: Mapping[str, object]) -> bool:
        included = {str(item) for item in spec.get("include_categories", ())}
        excluded = {str(item) for item in spec.get("exclude", ())}
        category = record.metadata.get("category_dir") or record.metadata.get("subdir")
        locator_parts = [part for part in (record.source_locator or "").replace("\\", "/").split("/") if part]
        category = category or next((part for part in locator_parts if part in included), None)
        filename = record.metadata.get("filename") or (locator_parts[-1] if locator_parts else "")
        return category in included and filename not in excluded

    @staticmethod
    def _nvd_allowed(record: SourceRecord, spec: Mapping[str, object], batch: SourceBatch) -> bool:
        filters = spec.get("filter")
        if not isinstance(filters, Mapping):
            return True
        days = filters.get("nvd_lookback_days")
        published = record.metadata.get("published")
        if not published or "T" not in str(published):
            try:
                obj = json.loads(record.original_text)
                raw_published = obj.get("cve", {}).get("published") if isinstance(obj, Mapping) else None
                published = raw_published or record.metadata.get("published_date")
            except (TypeError, json.JSONDecodeError):
                published = record.metadata.get("published_date")
        if not isinstance(days, int) or isinstance(days, bool) or days < 1 or not published or not batch.fetched_at:
            return False
        try:
            end = datetime.fromisoformat(batch.fetched_at.replace("Z", "+00:00"))
            start = datetime.fromisoformat(str(published).replace("Z", "+00:00"))
        except ValueError:
            return False
        if end.tzinfo is None:
            return False
        # NVD's API filters publication timestamps in UTC but some returned
        # CVE records omit an explicit offset. Match the provider's canonical
        # interpretation instead of rejecting every such record at import.
        if start.tzinfo is None:
            start = start.replace(tzinfo=timezone.utc)
        else:
            start = start.astimezone(timezone.utc)
        end = end.astimezone(timezone.utc)
        return end - timedelta(days=days) <= start <= end

    @staticmethod
    def _exploitdb_record(record: SourceRecord) -> tuple[str, Mapping[str, object], str | None]:
        normalized = {str(key).strip().casefold(): value for key, value in record.metadata.items()}
        forbidden = [key for key in normalized if key not in _EXPLOIT_FIELDS and any(part in key for part in _EXPLOIT_CODE_KEYS)]
        if forbidden:
            return "", {}, "exploitdb_record_contains_poc_or_source_fields"
        safe = {key: normalized[key] for key in _EXPLOIT_FIELDS if key in normalized}
        safe.setdefault("id", record.source_id)
        if str(safe["id"]) != record.source_id:
            return "", {}, "exploitdb_record_id_mismatch"
        if not any(safe.get(key) for key in ("description", "date_published", "platform", "type", "port", "codes")):
            return "", {}, "exploitdb_record_has_no_allowed_metadata"
        # The raw row is intentionally ignored; only this allowlist is stored.
        return json.dumps(safe, ensure_ascii=False, sort_keys=True, separators=(",", ":")), safe, None

    @staticmethod
    def _metadata(
        source: Mapping[str, object], family: str, spec: Mapping[str, object],
        batch: SourceBatch, profile: str, role: str,
    ) -> dict[str, str | int | float | bool | None]:
        result = {str(key): _scalar(value) for key, value in source.items() if str(key) != "original_text"}
        result.update({
            "source_family": family,
            "source_kind": _string(spec.get("kind")),
            "profile": profile,
            "document_role": role,
            "license_status": _string(spec.get("license_status")),
            "license_evidence": _string(spec.get("license_evidence")),
            "dataset_terms": _string(spec.get("dataset_terms")),
            "content_scope": _string(spec.get("content_scope")),
            "manifest_filters": _scalar(RedAmonSourceAdapter._filters(family, spec)),
            "applied_filters": _scalar(batch.applied_filters),
            "artifact_sha256": batch.artifact_sha256,
        })
        return dict(sorted(result.items()))

    @staticmethod
    def _count_blockers(
        batch: SourceBatch, expected: int | None, fetched: int | None,
        parsed: int | None, rejected: int | None, content_records: int,
    ) -> list[str]:
        blockers = []
        raw = (batch.expected_count, batch.fetched_count, batch.parsed_count, batch.rejected_count)
        if any(value is not None and _count(value) is None for value in raw):
            blockers.append("invalid_source_count")
        if expected is None:
            blockers.append("exact_expected_count_unavailable")
        elif fetched is None or fetched != expected:
            blockers.append("fetched_count_does_not_match_exact_expected_count")
        if fetched is None or parsed is None:
            blockers.append("fetch_or_parse_count_unavailable")
        elif parsed != fetched:
            blockers.append("parsed_count_does_not_match_fetched_count")
        if parsed is not None and parsed != content_records:
            blockers.append("parsed_count_does_not_match_returned_content_records")
        if rejected is None:
            blockers.append("rejected_count_unavailable")
        elif rejected:
            blockers.append("provider_reported_parse_rejections")
        return blockers

    @staticmethod
    def _coverage(
        family: str, state: str, *, blockers: Sequence[str] = (),
        requested_filters: Mapping[str, object] | None = None,
        applied_filters: Mapping[str, object] | None = None,
        source_metadata: Mapping[str, object] | None = None,
        source_revision: str | None = None, expected_count: int | None = None,
        fetched_count: int | None = 0, parsed_count: int | None = 0,
        rejected_count: int | None = 0, document_count: int = 0,
        chunk_count: int = 0, license_notice_count: int = 0,
        artifact_sha256: str | None = None, limitations: Sequence[str] = (),
    ) -> dict[str, object]:
        return {
            "source_name": family,
            "state": state,
            "license_status": _string((source_metadata or {}).get("license_status")),
            "source_revision": source_revision,
            "expected_count": expected_count,
            "fetched_count": fetched_count,
            "parsed_count": parsed_count,
            "rejected_count": rejected_count,
            "document_count": document_count,
            "chunk_count": chunk_count,
            "license_notice_count": license_notice_count,
            "artifact_sha256": artifact_sha256,
            "blockers": list(blockers),
            "limitations": list(limitations),
            "requested_filters": dict(requested_filters or {}),
            "applied_filters": dict(applied_filters or {}),
            "source_metadata": dict(source_metadata or {}),
            "resolution_path": _RESOLUTION.get(family),
        }


def _string(value: object) -> str | None:
    if value is None:
        return None
    value = str(value).strip()
    return value or None


def _scalar(value: object) -> str | int | float | bool | None:
    if value is None or isinstance(value, (str, int, float, bool)):
        return value
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"), default=str)


def _count(value: object) -> int | None:
    if value is None or isinstance(value, bool) or not isinstance(value, int) or value < 0:
        return None
    return value
