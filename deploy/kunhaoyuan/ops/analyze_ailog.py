#!/usr/bin/env python3
"""Strict conformance check of the router's AI access log against the
third-party gateway token-log spec (v1.2), section 8 checklist plus the
mapping rules that can be verified mechanically.

check-ailog.py answers "does the two-layer contract hold". This answers
"would the collector derive the right numbers from it", which is a longer
list: token arithmetic, cache-detail shape, duration units, and the
fields that must be absent.

usage: analyze_ailog.py <file.jsonl> [--sample N]
"""
import json
import re
import sys
from collections import Counter, defaultdict
from datetime import datetime

RFC3339 = re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$")
GATEWAY_TYPE = "nightgw"
TOKEN_KEYS = ("input_token", "output_token", "total_token")
# Field names that would carry prompt text. The spec forbids them in ai_log.
FORBIDDEN_INNER = ("question", "answer", "content", "messages", "prompt", "input", "output")
# Business string fields where "none" is the documented placeholder.
NONE_ALLOWED = (
    "request_model", "inference_id", "user_id", "ram_id", "customer_id",
    "agent_id", "instance_id", "cluster_instance", "billing_period",
    "encrypted_api_key", "vendor_encrypted_api_key", "failover_model",
)

fails = Counter()
examples = defaultdict(list)


def note(kind, detail, index=None, stamp=""):
    fails[kind] += 1
    if len(examples[kind]) < 3:
        where = "line %s" % index if index else "-"
        examples[kind].append("%s %s: %s" % (where, stamp, detail))


def num(value):
    """Return a float for a JSON number or a numeric string, else None."""
    if isinstance(value, bool):
        return None
    if isinstance(value, (int, float)):
        return float(value)
    if isinstance(value, str):
        try:
            return float(value)
        except ValueError:
            return None
    return None


def main(path, sample):
    stats = Counter()
    paths = Counter()
    statuses = Counter()
    models = Counter()
    user_agents = Counter()
    stamps = []
    no_token_streams = []
    cached_hist = Counter()
    durations = []
    first_seen = {}
    dupes = []
    prefixes = Counter()
    detail_keys = Counter()
    out_detail_keys = Counter()
    inner_key_sets = Counter()

    with open(path, "rb") as raw_file:
        head = raw_file.read(3)
        if head.startswith(b"\xef\xbb\xbf"):
            note("outer has a UTF-8 BOM", "the first line starts with EF BB BF")
    with open(path, "r", encoding="utf-8", errors="replace") as handle:
        for index, raw in enumerate(handle, 1):
            line = raw.rstrip("\n")
            if not line.strip():
                continue
            stats["lines"] += 1
            # 8.1 one complete JSON object per line
            try:
                outer = json.loads(line)
            except ValueError as exc:
                note("outer JSON does not parse", str(exc), index)
                continue
            if not isinstance(outer, dict):
                note("outer JSON is not an object", type(outer).__name__, index)
                continue
            stats["outer_ok"] += 1
            stamp = str(outer.get("start_time") or "")
            stamps.append(stamp)

            # 3. required outer fields
            for key in ("ai_log", "request_id", "response_code"):
                if key not in outer:
                    note("outer field missing", key, index, stamp)
            duration = num(outer.get("duration"))
            if "duration" in outer:
                if duration is None:
                    note("outer duration is not numeric", repr(outer.get("duration")), index, stamp)
                else:
                    durations.append(duration)
            else:
                note("outer duration missing", "recommended but absent", index, stamp)

            code = outer.get("response_code")
            code_num = num(code)
            if code_num is None:
                note("response_code is not numeric", repr(code), index, stamp)
            else:
                statuses[int(code_num)] += 1
                if code_num > 599:
                    note("response_code out of range", repr(code), index, stamp)

            # start_time: RFC3339 with a zone, parseable
            start = outer.get("start_time")
            if start is None:
                note("start_time missing", "startTime would be null", index, stamp)
            elif not isinstance(start, str) or not RFC3339.match(start):
                note("start_time is not RFC3339 with a zone", repr(start), index, stamp)
            else:
                try:
                    datetime.strptime(start.replace("Z", "+0000"),
                                      "%Y-%m-%dT%H:%M:%S.%f%z" if "." in start
                                      else "%Y-%m-%dT%H:%M:%S%z")
                except ValueError as exc:
                    note("start_time does not parse", str(exc), index, stamp)

            # 7.3 request_id is the ES document id
            request_id = outer.get("request_id")
            if request_id in (None, "", "none"):
                note("request_id is a placeholder", repr(request_id), index, stamp)
            else:
                if request_id in first_seen:
                    dupes.append(request_id)
                    note("request_id duplicated", "%s first at line %d"
                         % (request_id, first_seen[request_id]), index, stamp)
                else:
                    first_seen[request_id] = index
                prefixes[request_id.split("-")[0]] += 1

            for key in ("method", "path", "user_agent", "upstream_cluster",
                        "response_flags", "response_code_details",
                        "caller_request_id"):
                if key in outer and not isinstance(outer[key], str):
                    note("outer field is not a string", "%s = %r" % (key, outer[key]), index, stamp)
            # The caller's own id is a correlation aid, not a key. If the two
            # ever come out equal the router is trusting a header again, and a
            # replayed value would silently overwrite an earlier request's usage.
            if outer.get("caller_request_id") == request_id:
                note("caller id became the document id",
                     "%r - the request id must be generated" % request_id, index, stamp)
            if isinstance(outer.get("path"), str):
                paths[outer["path"]] += 1
            if isinstance(outer.get("user_agent"), str):
                user_agents[outer["user_agent"]] += 1

            # 8.2/8.3 ai_log is an escaped JSON string
            ai_raw = outer.get("ai_log")
            if ai_raw == "-":
                stats["non_ai"] += 1
                continue
            if not isinstance(ai_raw, str):
                note("ai_log is not a string", type(ai_raw).__name__, index, stamp)
                continue
            if ai_raw.strip().startswith('"'):
                note("ai_log looks double-encoded", ai_raw[:40], index, stamp)
            try:
                inner = json.loads(ai_raw)
            except ValueError as exc:
                note("inner ai_log does not parse", str(exc), index, stamp)
                continue
            if not isinstance(inner, dict):
                note("inner ai_log is not an object", type(inner).__name__, index, stamp)
                continue
            stats["inner_ok"] += 1
            inner_key_sets[tuple(sorted(inner))] += 1

            # 4.1 required inner fields
            if inner.get("gateway_type") != GATEWAY_TYPE:
                note("gateway_type is wrong", repr(inner.get("gateway_type")), index, stamp)
            if not isinstance(inner.get("request_model"), str):
                note("request_model is not a string", repr(inner.get("request_model")), index, stamp)
            else:
                models[inner["request_model"]] += 1
            response_type = inner.get("response_type")
            if response_type is not None and response_type not in ("stream", "normal"):
                note("response_type is not stream/normal", repr(response_type), index, stamp)

            # 8.9 llm-response: only for Claude, and it decides cache add-back
            llm_response = inner.get("llm-response")
            if llm_response == "claude":
                note("llm-response says claude on an OpenAI-compatible gateway",
                     "the collector would add cache tokens back to promptTokens", index, stamp)
            elif llm_response is not None and llm_response not in ("", None):
                note("llm-response has an unexpected value", repr(llm_response), index, stamp)

            # 2.6 no prompt or completion text
            for key in FORBIDDEN_INNER:
                if key in inner:
                    note("inner field carries request text", key, index, stamp)

            # numeric fields must be numbers, never "none"
            for key in TOKEN_KEYS + ("llm_service_duration", "llm_first_token_duration",
                                     "token_multiplier"):
                if key in inner and num(inner[key]) is None:
                    note("inner numeric field is not numeric", "%s = %r" % (key, inner[key]),
                         index, stamp)
            for key in NONE_ALLOWED:
                if inner.get(key) == "":
                    note("business field is empty string", key, index, stamp)

            # 4.2/8.3 details must be nested objects with numeric leaves
            for key in ("input_token_details", "output_token_details"):
                if key not in inner:
                    continue
                details = inner[key]
                if not isinstance(details, dict):
                    note("%s is not a nested object" % key, type(details).__name__, index, stamp)
                    continue
                for name, value in details.items():
                    if key == "input_token_details":
                        detail_keys[name] += 1
                    else:
                        out_detail_keys[name] += 1
                    if value is None:
                        note("%s.%s is null" % (key, name),
                             "the spec says to omit unknown numbers, not send null", index, stamp)
                    elif name == "cache_type":
                        if value == "":
                            note("cache_type is an empty string",
                                 "explicit-cache marker must be non-empty or absent", index, stamp)
                    elif num(value) is None:
                        note("%s.%s is not numeric" % (key, name), repr(value), index, stamp)
            details = inner.get("input_token_details") or {}
            if isinstance(details, dict):
                cached = num(details.get("cached_tokens"))
                if cached is not None:
                    cached_hist[int(cached)] += 1

            # 8.14 the token triple is all-or-nothing, and never zero-filled
            present = [key for key in TOKEN_KEYS if key in inner]
            if len(present) == 3:
                stats["with_tokens"] += 1
                tokens = dict((key, num(inner[key])) for key in TOKEN_KEYS)
                if any(value is None for value in tokens.values()):
                    note("token field is not numeric", str(tokens), index, stamp)
                else:
                    input_token = tokens["input_token"]
                    output_token = tokens["output_token"]
                    total_token = tokens["total_token"]
                    if input_token < 0 or output_token < 0:
                        note("negative token count", str(tokens), index, stamp)
                    # OpenAI-compatible: total is prompt + completion. The
                    # Claude add-back does not apply because llm-response is
                    # not "claude" here.
                    if abs(total_token - (input_token + output_token)) > 0:
                        note("total_token != input_token + output_token",
                             "%d != %d + %d" % (total_token, input_token, output_token),
                             index, stamp)
                    cached = num(details.get("cached_tokens")) if isinstance(details, dict) else None
                    cache_creation = num(details.get("cache_creation_input_tokens")) \
                        if isinstance(details, dict) else None
                    cache_read = num(details.get("cache_read_input_tokens")) \
                        if isinstance(details, dict) else None
                    for label, value in (("cached_tokens", cached),
                                         ("cache_creation_input_tokens", cache_creation),
                                         ("cache_read_input_tokens", cache_read)):
                        if value is not None and value > input_token:
                            note("%s exceeds input_token" % label,
                                 "%d > %d" % (value, input_token), index, stamp)
                    # 8.9 OpenAI-compatible input already contains the hits
                    if cached is not None and cached + (cache_creation or 0) > input_token:
                        note("cache tokens double-counted against input_token",
                             str(tokens), index, stamp)
            elif present:
                note("partial token triple", ", ".join(present), index, stamp)

            # durations: milliseconds, and first token cannot exceed total
            service = num(inner.get("llm_service_duration"))
            first = num(inner.get("llm_first_token_duration"))
            if service is None and "llm_service_duration" not in inner:
                note("llm_service_duration missing",
                     "the collector would fall back to the outer duration", index, stamp)
            if first is not None and service is not None and first > service + 1:
                note("first token duration exceeds service duration",
                     "%d > %d" % (first, service), index, stamp)
            if service is not None and duration is not None:
                stats["service_le_outer"] += 1 if service <= duration + 1 else 0

            # 5. patch must not be present unless interruption is detectable
            if "ai_log_token_patch" in outer:
                note("ai_log_token_patch is emitted", "we cannot detect interruptions",
                     index, stamp)

            if response_type == "stream" and len(present) < 3:
                no_token_streams.append((stamp, outer.get("path"), code, index))
                stats["stream_without_tokens"] += 1

    # ---- report
    total = stats["lines"]
    print("lines .................. %d" % total)
    print("outer parsed ........... %d" % stats["outer_ok"])
    print("inner parsed ........... %d" % stats["inner_ok"])
    print("non-AI ('-') ........... %d" % stats["non_ai"])
    print("with token triple ...... %d" % stats["with_tokens"])
    print("stream w/o tokens ...... %d" % stats["stream_without_tokens"])
    print("unique request ids ..... %d" % len(first_seen))
    if dupes:
        print("duplicate ids .......... %d" % len(dupes))
    print("request id prefixes .... %s" % dict(prefixes))
    print("status codes ........... %s" % dict(statuses.most_common(8)))
    print("paths .................. %s" % dict(paths.most_common(8)))
    print("models ................. %s" % dict(models.most_common(8)))
    print("user agents ............ %s" % dict(user_agents.most_common(6)))
    print("input detail keys ...... %s" % dict(detail_keys))
    print("output detail keys ..... %s" % dict(out_detail_keys))
    if stamps:
        print("time window ............ %s .. %s" % (min(stamps), max(stamps)))
    if durations:
        ordered = sorted(durations)
        print("outer duration ms ...... min %d p50 %d p95 %d max %d"
              % (ordered[0], ordered[len(ordered) // 2],
                 ordered[int(len(ordered) * 0.95)], ordered[-1]))
    if cached_hist:
        hits = sum(count for value, count in cached_hist.items() if value > 0)
        print("cached_tokens .......... zero %d, positive %d, max %d"
              % (cached_hist.get(0, 0), hits, max(cached_hist)))
    print("distinct inner shapes .. %d" % len(inner_key_sets))
    for shape, count in inner_key_sets.most_common(3):
        print("  %5d x %s" % (count, ",".join(shape)))

    if no_token_streams:
        print("\nstream without usage, by path/status:")
        bucket = Counter((path, code) for _, path, code, _ in no_token_streams)
        for key, count in bucket.most_common(6):
            print("  %5d x %s" % (count, key))
        print("  first 3 (%s): %s" % (len(no_token_streams), no_token_streams[:3]))
        print("  last  3: %s" % (no_token_streams[-3:],))

    if fails:
        print("\nFAILED %d distinct check(s), %d occurrence(s):"
              % (len(fails), sum(fails.values())))
        for kind, count in fails.most_common():
            print("  [%d] %s" % (count, kind))
            for example in examples[kind]:
                print("        %s" % example)
        return 1
    print("\nPASS: every line satisfies the two-layer contract and the token arithmetic")
    return 0


if __name__ == "__main__":
    if len(sys.argv) < 2:
        print(__doc__)
        raise SystemExit(2)
    raise SystemExit(main(sys.argv[1], None))
