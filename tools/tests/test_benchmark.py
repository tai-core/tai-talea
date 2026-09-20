import importlib.util
import io
import json
from pathlib import Path
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("bench", Path(__file__).parents[1]/"talea_benchmark.py")
bench = importlib.util.module_from_spec(spec)
spec.loader.exec_module(bench)


class BenchmarkTest(unittest.TestCase):
    def stream(self, usage=True, done=True):
        records = [dict(choices=[dict(delta=dict(role="assistant"))]),
                   dict(choices=[dict(delta=dict(content="hello"))]),
                   dict(choices=[dict(delta=dict(content=" world"))])]
        if usage:
            records.append(dict(choices=[], usage=dict(prompt_tokens=50, completion_tokens=5)))
        raw = b"".join(b"data: "+json.dumps(row).encode()+b"\n\n" for row in records)
        return io.BytesIO(raw+(b"data: [DONE]\n\n" if done else b""))

    def test_actual_usage_and_first_content_timing(self):
        with patch.object(bench,"open_request",return_value=self.stream()), patch.object(bench.time,"perf_counter",side_effect=[1,2,3,4]):
            result = bench.request_one("http://router", "model", "test", 64, 10)
        self.assertTrue(result["ok"])
        self.assertEqual((result["prompt_tokens"],result["completion_tokens"]),(50,5))
        self.assertEqual(result["ttft_s"],1)
        self.assertEqual(result["tpot_s"],.25)

    def test_missing_usage_and_truncated_stream_are_failures(self):
        for usage,done in [(False,True),(True,False)]:
            with patch.object(bench,"open_request",return_value=self.stream(usage,done)):
                self.assertFalse(bench.request_one("http://router","m","x",64,10)["ok"])

    def test_failed_requests_not_counted_in_success_throughput(self):
        good=dict(ok=True,prompt_tokens=50,completion_tokens=5,latency_s=2,ttft_s=.5,tpot_s=.1)
        report=bench.summarize([good,dict(ok=False)],4)
        self.assertEqual(report["requests_per_second"],.25)
        self.assertEqual(report["output_tokens_per_second"],1.25)
        self.assertEqual(report["failures"],1)

    def test_stream_keepalive_does_not_extend_deadline(self):
        with patch.object(bench,"open_request",return_value=io.BytesIO(b": ping\n\n")), patch.object(bench.time,"monotonic",side_effect=[0,11]):
            result=bench.request_one("http://router","m","x",64,10)
        self.assertFalse(result["ok"])
        self.assertIn("total deadline",result["error"])


if __name__=="__main__":
    unittest.main()
