"""Unit tests for snapshot.py's offer-file parsing (no network)."""

import pathlib
import sys
import unittest

sys.path.insert(0, str(pathlib.Path(__file__).parent))
import snapshot  # noqa: E402


def offer(*products):
    """An offer file with one OnDemand term per (usagetype, attrs, tiers) product."""
    doc = {"products": {}, "terms": {"OnDemand": {}}}
    for i, (usagetype, attrs, tiers) in enumerate(products):
        sku = f"SKU{i}"
        doc["products"][sku] = {"attributes": {"usagetype": usagetype, **attrs}}
        dims = {}
        for j, (begin, end, usd) in enumerate(tiers):
            dims[f"{sku}.d{j}"] = {"beginRange": begin, "endRange": end, "pricePerUnit": {"USD": usd}}
        doc["terms"]["OnDemand"][sku] = {f"{sku}.t": {"priceDimensions": dims}}
    return doc


class SnapshotTest(unittest.TestCase):
    def test_region_prefixes_are_stripped_from_usage_types(self):
        self.assertEqual(snapshot.base_usagetype("EUW1-Storage-ShardHour"), "Storage-ShardHour")
        self.assertEqual(snapshot.base_usagetype("EU-Requests-Tier1"), "Requests-Tier1")
        self.assertEqual(snapshot.base_usagetype("Requests-RBP"), "Requests-RBP")
        self.assertEqual(snapshot.base_usagetype("LongTermRetention-ReadBytes"), "LongTermRetention-ReadBytes")

    def test_tiers_are_sorted_and_the_last_is_unbounded(self):
        doc = offer(("Requests-RBP", {}, [("100000000000", "Inf", "0.0000003"), ("0", "100000000000", "0.0000004")]))
        tiers = snapshot.tiers_for(doc, snapshot.PRICES["sqs-standard-requests"])
        self.assertEqual(tiers, [{"from": 0.0, "to": 1e11, "usd": 4e-7}, {"from": 1e11, "to": 0, "usd": 3e-7}])

    def test_attributes_narrow_a_usage_type_shared_by_several_products(self):
        doc = offer(
            ("USE1-Event-64K-Chunks", {"operation": "ReceivedPartnerEvents"}, [("0", "Inf", "0.000002")]),
            ("USE1-Event-64K-Chunks", {"operation": "PutEvents"}, [("0", "Inf", "0.000001")]),
        )
        tiers = snapshot.tiers_for(doc, snapshot.PRICES["eventbridge-custom-events"])
        self.assertEqual(tiers, [{"from": 0.0, "to": 0, "usd": 1e-6}])

    def test_two_different_prices_for_one_item_are_an_error(self):
        doc = offer(
            ("Storage-ShardHour", {}, [("0", "Inf", "0.015")]),
            ("Storage-ShardHour", {}, [("0", "Inf", "0.017")]),
        )
        with self.assertRaises(SystemExit):
            snapshot.tiers_for(doc, snapshot.PRICES["kinesis-shard-hours"])

    def test_a_missing_item_is_none(self):
        self.assertIsNone(snapshot.tiers_for(offer(), snapshot.PRICES["msk-storage"]))


if __name__ == "__main__":
    unittest.main()
