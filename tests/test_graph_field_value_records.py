from __future__ import annotations

import pytest

from bb_assistant.behavior.vendor import graph
from bb_assistant.behavior.vendor.common import BehaviorError, entity_id


def test_project_keyed_field_occurrence_and_value_records_are_accepted(tmp_path) -> None:
    receipt = tmp_path / "receipt.json"
    receipt.write_text("synthetic receipt", encoding="utf-8")
    source = {"path": str(receipt), "sha256": "a" * 64, "record_locator": "$.records[0]"}
    declared = {(source["path"], source["sha256"])}
    program = "b" * 64
    occurrence_data = {
        "program": program,
        "capture_id": "c" * 64,
        "revision_id": "d" * 64,
        "observation_id": "observation-" + "e" * 64,
        "parameter_id": "parameter-" + "f" * 64,
        "side": "request",
        "location": "query",
        "path": 'query["item"][0]',
        "occurrence_index": 0,
        "type": "string",
        "source_span": {
            "start": 10,
            "end": 25,
            "unit": "byte",
            "offset_basis": "request_message",
            "wire_fidelity": "source_bytes",
        },
        "value_span": {
            "start": 15,
            "end": 25,
            "unit": "byte",
            "offset_basis": "request_message",
            "wire_fidelity": "source_bytes",
        },
    }
    occurrence = {
        "id": "field_occurrence-" + "1" * 64,
        "kind": "field_occurrence",
        "data": occurrence_data,
        "source_refs": [source],
    }
    normalized = graph._entity(occurrence, declared, program)
    assert normalized["data"] == occurrence_data

    value_reference = {
        "id": "value_reference-" + "2" * 64,
        "kind": "value_reference",
        "data": {
            "program": program,
            "token_version": "field-equality-v1",
            "value_type": "string",
            "normalization": "urlencoded_scalar_v1",
            "equality_signature": "3" * 64,
        },
        "source_refs": [source],
    }
    assert graph._entity(value_reference, declared, program)["kind"] == "value_reference"

    identity = {
        "subject": value_reference["id"],
        "predicate": "value_used_in",
        "object": occurrence["id"],
        "context": {"program": program, "scenario": "keyed_value_equality", "scenario_step": "correlation_only"},
        "origin_kind": "parsed",
        "evidence_state": "observed",
        "source_refs": [source],
    }
    assertion = {"id": entity_id("assertion", identity), **identity}
    assert graph._assertion(assertion, declared, program)["predicate"] == "value_used_in"


def test_field_occurrence_rejects_out_of_bounds_value_span(tmp_path) -> None:
    receipt = tmp_path / "receipt.json"
    receipt.write_text("synthetic receipt", encoding="utf-8")
    source = {"path": str(receipt), "sha256": "a" * 64, "record_locator": "$.records[0]"}
    data = {
        "program": "b" * 64,
        "capture_id": "c" * 64,
        "revision_id": "d" * 64,
        "observation_id": "observation-" + "e" * 64,
        "parameter_id": "parameter-" + "f" * 64,
        "side": "request",
        "location": "query",
        "path": 'query["item"][]',
        "occurrence_index": 0,
        "type": "string",
        "source_span": {"start": 10, "end": 25, "unit": "byte", "offset_basis": "request_message", "wire_fidelity": "source_bytes"},
        "value_span": {"start": 11, "end": 26, "unit": "byte", "offset_basis": "request_message", "wire_fidelity": "source_bytes"},
    }
    occurrence = {"id": "field_occurrence-" + "1" * 64, "kind": "field_occurrence", "data": data, "source_refs": [source]}
    with pytest.raises(BehaviorError):
        graph._entity(occurrence, {(source["path"], source["sha256"])}, data["program"])
