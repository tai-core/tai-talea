#!/usr/bin/env python3
"""Conformance check for the router's AI access log against the nightgw spec.

Reads one access-log line per line (the `{"ai_log":...}` lines only) and reports
per-rule pass/fail, in the order of the spec's section 8 checklist. Written for
python3.6 compatibility because the jump host is not guaranteed to be newer.
"""
import json
import re
import sys
from datetime import datetime

RFC3339 = re.compile(
    r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$"
)
REQUIRED_OUTER = ("ai_log", "request_id", "response_code")
REQUIRED_INNER = ("gateway_type",)
TOKEN_KEYS = ("input_token", "output_token", "total_token")

failures = []
warnings = []
# Set per line so a failure can say which request it came from.
current_stamp = ""
counts = {
    "lines": 0,
    "parsed": 0,
    "inner_parsed": 0,
    "non_ai": 0,
    "with_tokens": 0,
    "stream": 0,
    "stream_without_tokens": 0,
    "refused_stream": 0,
}


def fail(index, message):
    failures.append("%s: %s" % (location(index), message))


def warn(index, message):
    warnings.append("%s: %s" % (location(index), message))


def location(index):
    """Line number plus the request's own timestamp.

    The log is one append-only file, so a window exported right after a gateway
    upgrade still holds lines the previous binary wrote. Without the timestamp a
    failure from last week looks exactly like a regression from a minute ago.
    """
    if current_stamp:
        return "line %d (%s)" % (index, current_stamp)
    return "line %d" % index


def is_number(value):
    return isinstance(value, (int, float)) or (
        isinstance(value, str) and value.lstrip("-").isdigit()
    )


def main(path):
    seen_request_ids = {}
    with open(path, "r", encoding="utf-8", errors="replace") as handle:
        for index, raw in enumerate(handle, 1):
            global current_stamp
            current_stamp = ""
            line = raw.rstrip("\n")
            if not line.strip():
                continue
            counts["lines"] += 1

            # 1. one complete JSON object per line
            try:
                outer = json.loads(line)
            except ValueError as exc:
                fail(index, "outer JSON does not parse (%s)" % exc)
                continue
            if not isinstance(outer, dict):
                fail(index, "outer JSON is not an object")
                continue
            counts["parsed"] += 1
            current_stamp = str(outer.get("start_time") or "")

            # 2/3. the three fields the collector treats as mandatory
            for key in REQUIRED_OUTER:
                if key not in outer:
                    fail(index, "outer field %s is missing" % key)

            ai_raw = outer.get("ai_log")
            if ai_raw == "-":
                counts["non_ai"] += 1
                continue
            if not isinstance(ai_raw, str):
                fail(index, "ai_log is %s, the spec requires an escaped JSON string"
                     % type(ai_raw).__name__)
                continue

            # 4. the inner object must parse
            try:
                inner = json.loads(ai_raw)
            except ValueError as exc:
                fail(index, "inner ai_log JSON does not parse (%s)" % exc)
                continue
            if not isinstance(inner, dict):
                fail(index, "inner ai_log is not an object")
                continue
            counts["inner_parsed"] += 1

            for key in REQUIRED_INNER:
                if key not in inner:
                    fail(index, "inner field %s is missing" % key)
            if inner.get("gateway_type") not in (None, "nightgw"):
                fail(index, "gateway_type is %r, expected nightgw" % inner.get("gateway_type"))

            # 5. request_id is the idempotency key: unique, present, not a placeholder
            request_id = outer.get("request_id")
            if request_id in (None, "", "none"):
                fail(index, "request_id is %r" % request_id)
            elif request_id in seen_request_ids:
                fail(index, "request_id duplicates line %d" % seen_request_ids[request_id])
            else:
                seen_request_ids[request_id] = index

            # 6. durations are milliseconds and integers
            for key in ("duration",):
                if key in outer and not is_number(outer[key]):
                    fail(index, "outer %s is not a number: %r" % (key, outer[key]))
            for key in ("llm_service_duration", "llm_first_token_duration"):
                if key in inner and not is_number(inner[key]):
                    fail(index, "inner %s is not a number: %r" % (key, inner[key]))

            # 7. start_time must carry a zone
            start_time = outer.get("start_time")
            if start_time is not None:
                if not isinstance(start_time, str) or not RFC3339.match(start_time):
                    fail(index, "start_time is not RFC3339 with a zone: %r" % start_time)
                else:
                    try:
                        datetime.strptime(start_time.replace("Z", "+0000"), "%Y-%m-%dT%H:%M:%S.%f%z")
                    except ValueError:
                        try:
                            datetime.strptime(start_time.replace("Z", "+0000"), "%Y-%m-%dT%H:%M:%S%z")
                        except ValueError as exc:
                            fail(index, "start_time does not parse (%s)" % exc)

            # 8. token details must be nested objects, never escaped strings
            for key in ("input_token_details", "output_token_details"):
                if key in inner and not isinstance(inner[key], dict):
                    fail(index, "%s is %s, the spec requires a nested object"
                         % (key, type(inner[key]).__name__))

            # 9. the token triple is all-or-nothing, and its absence is the
            #    stream-without-usage case rather than a conformance bug
            present = [key for key in TOKEN_KEYS if key in inner]
            if len(present) == 3:
                counts["with_tokens"] += 1
            elif present:
                fail(index, "partial token triple: %s" % ", ".join(present))
            if inner.get("response_type") == "stream":
                counts["stream"] += 1
                if len(present) < 3:
                    # A refusal never had a usage to report: the pool limit
                    # answers 429 and an empty pool answers 503 before any
                    # generation starts, and the spec says to leave the token
                    # fields out rather than write zeros. Only a 2xx stream with
                    # no usage is evidence that capture is broken.
                    code = outer.get("response_code")
                    try:
                        code = int(code)
                    except (TypeError, ValueError):
                        code = None
                    if code is not None and 200 <= code < 300:
                        counts["stream_without_tokens"] += 1
                    else:
                        counts["refused_stream"] += 1

            # 10. unknown usage must be omitted, never zero-filled
            if len(present) == 3:
                if all(inner.get(key) == 0 for key in TOKEN_KEYS):
                    warn(index, "all three token fields are 0; verify this is a real measurement")

    print("lines read ............. %d" % counts["lines"])
    print("outer JSON parsed ...... %d" % counts["parsed"])
    print("inner ai_log parsed .... %d" % counts["inner_parsed"])
    print("non-AI lines ........... %d" % counts["non_ai"])
    print("lines with a token set . %d" % counts["with_tokens"])
    print("stream lines ........... %d (of which 2xx without tokens: %d, refused: %d)"
          % (counts["stream"], counts["stream_without_tokens"], counts["refused_stream"]))
    print("unique request ids ..... %d" % len(seen_request_ids))

    if counts["stream_without_tokens"]:
        print("\nWARNING: %d answered 2xx stream(s) carry no usage. The Router is run"
              % counts["stream_without_tokens"])
        print("         with --force-stream-usage, so this should not happen for")
        print("         traffic that went through it: check whether these lines")
        print("         predate that flag or came from another gateway.")
    for message in warnings[:5]:
        print("WARN  " + message)
    if failures:
        print("\nFAILED %d check(s):" % len(failures))
        for message in failures[:10]:
            print("  " + message)
        return 1
    print("\nPASS: every line satisfies the two-layer contract")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1]))
