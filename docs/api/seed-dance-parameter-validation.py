#!/usr/bin/env python3
"""Run the Seed Dance public HTTP parameter contract without storing secrets.

The default matrix contains only requests that must fail before an upstream
video task is submitted. It is therefore suitable for validating a deployed
instance without intentionally creating billable work.
"""

from __future__ import annotations

import argparse
import base64
import binascii
import getpass
import json
import os
import struct
import sys
import urllib.error
import urllib.request
import uuid
import zlib
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any


MODEL = "seedance-uncensored"


@dataclass(frozen=True)
class ExpectedError:
    status: int
    codes: frozenset[str]
    types: frozenset[str] | None = None


@dataclass
class Result:
    name: str
    status: int
    code: str | None
    error_type: str | None
    passed: bool


def make_png(
    width: int,
    height: int,
    rgb: tuple[int, int, int] = (20, 40, 60),
) -> bytes:
    signature = b"\x89PNG\r\n\x1a\n"

    def chunk(kind: bytes, data: bytes) -> bytes:
        checksum = binascii.crc32(kind + data) & 0xFFFFFFFF
        return (
            struct.pack(">I", len(data))
            + kind
            + data
            + struct.pack(">I", checksum)
        )

    ihdr = struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0)
    scanline = b"\x00" + bytes(rgb) * width
    pixels = scanline * height
    return (
        signature
        + chunk(b"IHDR", ihdr)
        + chunk(b"IDAT", zlib.compress(pixels, 9))
        + chunk(b"IEND", b"")
    )


def multipart_body(
    fields: list[tuple[str, str]],
    files: list[tuple[str, str, str, bytes]],
) -> tuple[bytes, str]:
    boundary = "----seedance-" + uuid.uuid4().hex
    body = bytearray()
    for name, value in fields:
        body.extend(
            (
                f"--{boundary}\r\n"
                f'Content-Disposition: form-data; name="{name}"\r\n\r\n'
                f"{value}\r\n"
            ).encode()
        )
    for name, filename, content_type, data in files:
        body.extend(
            (
                f"--{boundary}\r\n"
                f'Content-Disposition: form-data; name="{name}"; '
                f'filename="{filename}"\r\n'
                f"Content-Type: {content_type}\r\n\r\n"
            ).encode()
        )
        body.extend(data)
        body.extend(b"\r\n")
    body.extend(f"--{boundary}--\r\n".encode())
    return bytes(body), f"multipart/form-data; boundary={boundary}"


def decode_error(raw: bytes) -> tuple[str | None, str | None, str | None]:
    try:
        payload = json.loads(raw)
    except (UnicodeDecodeError, json.JSONDecodeError):
        return None, None, None
    if not isinstance(payload, dict):
        return None, None, None
    error = payload.get("error")
    if not isinstance(error, dict):
        return None, None, None
    message = error.get("message")
    error_type = error.get("type")
    code = error.get("code")
    return (
        message if isinstance(message, str) else None,
        error_type if isinstance(error_type, str) else None,
        code if isinstance(code, str) else None,
    )


def perform(
    *,
    base_url: str,
    token: str | None,
    method: str,
    path: str,
    data: bytes | None,
    content_type: str | None,
) -> tuple[int, bytes]:
    request = urllib.request.Request(
        base_url.rstrip("/") + path,
        data=data,
        method=method,
    )
    if token is not None:
        request.add_header("Authorization", "Bearer " + token)
    if content_type:
        request.add_header("Content-Type", content_type)
    request.add_header("Accept", "application/json")
    try:
        with urllib.request.urlopen(request, timeout=30) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as error:
        return error.code, error.read()


def validate_error(
    *,
    name: str,
    status: int,
    raw: bytes,
    expected: ExpectedError,
) -> Result:
    message, error_type, code = decode_error(raw)
    passed = (
        status == expected.status
        and code in expected.codes
        and message is not None
        and (expected.types is None or error_type in expected.types)
    )
    return Result(name, status, code, error_type, passed)


def invalid_json_cases() -> list[
    tuple[str, bytes | dict[str, Any], ExpectedError]
]:
    invalid_request = ExpectedError(400, frozenset({"invalid_request"}))
    invalid_duration = ExpectedError(400, frozenset({"invalid_duration"}))
    invalid_resolution = ExpectedError(400, frozenset({"invalid_resolution"}))
    invalid_image = ExpectedError(400, frozenset({"invalid_image"}))
    malformed_body = ExpectedError(
        400,
        frozenset({""}),
        frozenset({"new_api_error"}),
    )

    small = base64.b64encode(make_png(239, 240)).decode()
    valid_a = base64.b64encode(make_png(240, 240)).decode()
    valid_b = base64.b64encode(make_png(240, 240, (21, 40, 60))).decode()

    return [
        (
            "malformed_json",
            b'{"model":"seedance-uncensored",',
            malformed_body,
        ),
        (
            "metadata_not_object",
            {"model": MODEL, "prompt": "p", "metadata": []},
            invalid_request,
        ),
        ("prompt_missing", {"model": MODEL}, invalid_request),
        (
            "prompt_null",
            {"model": MODEL, "prompt": None},
            invalid_request,
        ),
        (
            "prompt_blank",
            {"model": MODEL, "prompt": "   "},
            invalid_request,
        ),
        (
            "prompt_non_string",
            {"model": MODEL, "prompt": 123},
            invalid_request,
        ),
        (
            "duration_zero",
            {"model": MODEL, "prompt": "p", "duration": 0},
            invalid_duration,
        ),
        (
            "duration_negative",
            {"model": MODEL, "prompt": "p", "duration": -1},
            invalid_duration,
        ),
        (
            "duration_over_max",
            {"model": MODEL, "prompt": "p", "duration": 16},
            invalid_duration,
        ),
        (
            "duration_fraction",
            {"model": MODEL, "prompt": "p", "duration": 1.5},
            invalid_duration,
        ),
        (
            "duration_exponent",
            (
                b'{"model":"seedance-uncensored","prompt":"p",'
                b'"duration":1e1}'
            ),
            invalid_duration,
        ),
        (
            "duration_empty_string",
            {"model": MODEL, "prompt": "p", "duration": ""},
            invalid_duration,
        ),
        (
            "duration_alpha",
            {"model": MODEL, "prompt": "p", "duration": "abc"},
            invalid_duration,
        ),
        (
            "duration_boolean",
            {"model": MODEL, "prompt": "p", "duration": True},
            invalid_duration,
        ),
        (
            "duration_object",
            {"model": MODEL, "prompt": "p", "duration": {}},
            invalid_duration,
        ),
        (
            "duration_conflict_top_level",
            {"model": MODEL, "prompt": "p", "duration": 1, "seconds": 2},
            invalid_duration,
        ),
        (
            "duration_conflict_metadata",
            {
                "model": MODEL,
                "prompt": "p",
                "duration": 1,
                "metadata": {"duration": 2},
            },
            invalid_duration,
        ),
        (
            "resolution_unsupported",
            {"model": MODEL, "prompt": "p", "size": "640x360"},
            invalid_resolution,
        ),
        (
            "resolution_non_string",
            {"model": MODEL, "prompt": "p", "size": 720},
            invalid_request,
        ),
        (
            "metadata_resolution_non_string",
            {
                "model": MODEL,
                "prompt": "p",
                "metadata": {"resolution": True},
            },
            invalid_request,
        ),
        (
            "resolution_conflict",
            {
                "model": MODEL,
                "prompt": "p",
                "size": "1280x720",
                "metadata": {"resolution": "1080P"},
            },
            invalid_resolution,
        ),
        (
            "t2v_480p",
            {"model": MODEL, "prompt": "p", "size": "854x480"},
            invalid_resolution,
        ),
        (
            "prompt_optimization_string",
            {
                "model": MODEL,
                "prompt": "p",
                "metadata": {"prompt_optimization": "false"},
            },
            invalid_request,
        ),
        (
            "multi_shot_number",
            {
                "model": MODEL,
                "prompt": "p",
                "metadata": {"multi_shot": 1},
            },
            invalid_request,
        ),
        (
            "strict_duration_object",
            {
                "model": MODEL,
                "prompt": "p",
                "metadata": {"strict_duration": {}},
            },
            invalid_request,
        ),
        (
            "negative_prompt_non_string",
            {
                "model": MODEL,
                "prompt": "p",
                "metadata": {"negative_prompt": 5},
            },
            invalid_request,
        ),
        (
            "image_invalid_base64",
            {"model": MODEL, "prompt": "p", "image": "%%%INVALID"},
            invalid_image,
        ),
        (
            "image_unsupported_data_uri",
            {
                "model": MODEL,
                "prompt": "p",
                "image": "data:image/gif;base64,R0lGODlh",
            },
            invalid_image,
        ),
        (
            "image_too_small",
            {"model": MODEL, "prompt": "p", "image": small},
            invalid_image,
        ),
        (
            "image_non_string",
            {"model": MODEL, "prompt": "p", "image": 123},
            invalid_image,
        ),
        (
            "images_not_array",
            {"model": MODEL, "prompt": "p", "images": {}},
            invalid_image,
        ),
        (
            "images_more_than_one",
            {
                "model": MODEL,
                "prompt": "p",
                "images": [valid_a, valid_a],
            },
            invalid_image,
        ),
        (
            "different_image_aliases",
            {
                "model": MODEL,
                "prompt": "p",
                "image": valid_a,
                "input_reference": valid_b,
            },
            invalid_image,
        ),
    ]


def invalid_multipart_cases() -> list[
    tuple[
        str,
        list[tuple[str, str]],
        list[tuple[str, str, str, bytes]],
        ExpectedError,
    ]
]:
    invalid_request = ExpectedError(400, frozenset({"invalid_request"}))
    invalid_image = ExpectedError(400, frozenset({"invalid_image"}))
    image = make_png(240, 240)
    return [
        (
            "multipart_duplicate_prompt",
            [("model", MODEL), ("prompt", "p"), ("prompt", "q")],
            [],
            invalid_request,
        ),
        (
            "multipart_boolean_not_text",
            [
                ("model", MODEL),
                ("prompt", "p"),
                ("metadata", '{"strict_duration":true}'),
            ],
            [],
            invalid_request,
        ),
        (
            "multipart_boolean_invalid_text",
            [
                ("model", MODEL),
                ("prompt", "p"),
                ("metadata", '{"multi_shot":"yes"}'),
            ],
            [],
            invalid_request,
        ),
        (
            "multipart_metadata_malformed",
            [("model", MODEL), ("prompt", "p"), ("metadata", "{")],
            [],
            invalid_request,
        ),
        (
            "multipart_empty_file",
            [("model", MODEL), ("prompt", "p")],
            [("input_reference", "empty.png", "image/png", b"")],
            invalid_image,
        ),
        (
            "multipart_unsupported_file_field",
            [("model", MODEL), ("prompt", "p")],
            [("image", "image.png", "image/png", image)],
            invalid_image,
        ),
        (
            "multipart_multiple_files",
            [("model", MODEL), ("prompt", "p")],
            [
                ("input_reference", "a.png", "image/png", image),
                ("input_reference", "b.png", "image/png", image),
            ],
            invalid_image,
        ),
    ]


def print_result(result: Result) -> None:
    print(
        f"{result.name:38} "
        f"http={result.status} "
        f"code={result.code!r} "
        f"type={result.error_type!r} "
        f"pass={result.passed}"
    )


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--base-url", required=True)
    parser.add_argument(
        "--token-env",
        default="NEW_API_TOKEN",
        help="environment variable to read before prompting",
    )
    parser.add_argument(
        "--report",
        default="/tmp/seedance-invalid-live-report.json",
    )
    args = parser.parse_args()

    token = os.environ.get(args.token_env)
    if not token:
        token = getpass.getpass("New API token: ")
    if not token:
        raise SystemExit("New API token is required")

    results: list[Result] = []
    unexpected_success = False
    post_matrix_aborted = False

    for name, body, expected in invalid_json_cases():
        data = (
            body
            if isinstance(body, bytes)
            else json.dumps(body, separators=(",", ":")).encode()
        )
        status, raw = perform(
            base_url=args.base_url,
            token=token,
            method="POST",
            path="/v1/videos",
            data=data,
            content_type="application/json",
        )
        result = validate_error(
            name=name,
            status=status,
            raw=raw,
            expected=expected,
        )
        results.append(result)
        print_result(result)
        if not result.passed:
            unexpected_success = 200 <= status < 300
            post_matrix_aborted = True
            break

    if not post_matrix_aborted:
        for name, fields, files, expected in invalid_multipart_cases():
            data, content_type = multipart_body(fields, files)
            status, raw = perform(
                base_url=args.base_url,
                token=token,
                method="POST",
                path="/v1/videos",
                data=data,
                content_type=content_type,
            )
            result = validate_error(
                name=name,
                status=status,
                raw=raw,
                expected=expected,
            )
            results.append(result)
            print_result(result)
            if not result.passed:
                unexpected_success = 200 <= status < 300
                post_matrix_aborted = True
                break

    if not post_matrix_aborted:
        status, raw = perform(
            base_url=args.base_url,
            token=token,
            method="GET",
            path="/v1/videos/TASK_DOES_NOT_EXIST",
            data=None,
            content_type=None,
        )
        result = validate_error(
            name="unknown_task",
            status=status,
            raw=raw,
            expected=ExpectedError(
                404,
                frozenset({"task_not_found"}),
                frozenset({"invalid_request_error"}),
            ),
        )
        results.append(result)
        print_result(result)

        for name, auth in [
            ("missing_token", None),
            ("invalid_token", "INVALID_TOKEN"),
        ]:
            status, raw = perform(
                base_url=args.base_url,
                token=auth,
                method="GET",
                path="/v1/videos/TASK_DOES_NOT_EXIST",
                data=None,
                content_type=None,
            )
            result = validate_error(
                name=name,
                status=status,
                raw=raw,
                expected=ExpectedError(
                    401,
                    frozenset({""}),
                    frozenset({"new_api_error"}),
                ),
            )
            results.append(result)
            print_result(result)

    report = {
        "target": "TARGET_HOST",
        "total": len(results),
        "passed": sum(result.passed for result in results),
        "failed": sum(not result.passed for result in results),
        "unexpected_success": unexpected_success,
        "post_matrix_aborted": post_matrix_aborted,
        "results": [asdict(result) for result in results],
    }
    report_path = Path(args.report)
    report_path.write_text(
        json.dumps(report, indent=2, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    print(
        "INVALID_MATRIX_COMPLETE "
        f"{report['passed']}/{report['total']} "
        f"report={report_path}"
    )
    return 0 if report["failed"] == 0 and not unexpected_success else 1


if __name__ == "__main__":
    sys.exit(main())
