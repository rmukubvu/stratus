"""Unit tests for the catalogue generator and the coverage probe's classification."""

from __future__ import annotations

import unittest

import coverage
import generate


class CompactModelTest(unittest.TestCase):
    MODEL = {
        "metadata": {"endpointPrefix": "sqs", "protocol": "json", "targetPrefix": "AmazonSQS", "apiVersion": "2012-11-05"},
        "operations": {
            "SendMessage": {
                "http": {"method": "POST", "requestUri": "/"},
                "input": {"shape": "SendMessageRequest"},
                "output": {"shape": "SendMessageResult"},
                "errors": [{"shape": "QueueDoesNotExist"}],
                "documentation": "<p>dropped</p>",
            }
        },
        "shapes": {
            "SendMessageRequest": {
                "type": "structure",
                "required": ["QueueUrl"],
                "members": {"QueueUrl": {"shape": "String", "documentation": "x"}, "Tags": {"shape": "TagMap"}},
            },
            "TagMap": {"type": "map", "key": {"shape": "TagKey"}, "value": {"shape": "String"}},
            "TagKey": {"type": "string", "min": 1, "max": 128, "pattern": "^[a-z]+$"},
            "String": {"type": "string"},
            "SendMessageResult": {"type": "structure", "members": {}},
            "QueueDoesNotExist": {"type": "structure", "members": {}},
        },
    }

    def test_keeps_operations_bindings_errors_and_input_constraints_only(self):
        out = generate.compact_model(self.MODEL)
        self.assertEqual(out["protocol"], "json")
        self.assertEqual(out["targetPrefix"], "AmazonSQS")
        self.assertEqual(
            out["operations"]["SendMessage"],
            {"http": {"method": "POST", "requestUri": "/"}, "input": "SendMessageRequest", "errors": ["QueueDoesNotExist"]},
        )
        self.assertEqual(set(out["shapes"]), {"SendMessageRequest", "TagMap", "TagKey", "String"})
        self.assertEqual(out["shapes"]["TagKey"], {"type": "string", "min": 1, "max": 128, "pattern": "^[a-z]+$"})
        self.assertEqual(out["shapes"]["SendMessageRequest"]["members"]["QueueUrl"], {"shape": "String"})


class ClassifyTest(unittest.TestCase):
    def test_only_501_and_not_implemented_codes_count_as_missing(self):
        self.assertEqual(coverage.classify(501, "Anything"), coverage.NOT_IMPLEMENTED)
        self.assertEqual(coverage.classify(400, "UnknownOperationException"), coverage.NOT_IMPLEMENTED)
        self.assertEqual(coverage.classify(400, "InvalidAction"), coverage.NOT_IMPLEMENTED)
        self.assertEqual(coverage.classify(404, "ResourceNotFoundException"), coverage.IMPLEMENTED)
        self.assertEqual(coverage.classify(200, ""), coverage.IMPLEMENTED)


if __name__ == "__main__":
    unittest.main()
