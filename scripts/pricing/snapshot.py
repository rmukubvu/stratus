#!/usr/bin/env python3
"""Build Stratus's dated AWS price snapshot from AWS's public price list.

AWS publishes every on-demand price as offer files under
https://pricing.us-east-1.amazonaws.com/offers/v1.0/aws/ (no account or
credentials needed). This script reads the per-region offer file of each
service Stratus prices, picks the few prices `stratus advise` uses (listed
in PRICES below) and writes them, with their publication date, to
internal/pricing/snapshot.json, which the binary embeds.

    python3 scripts/pricing/snapshot.py            # fetch and rewrite the snapshot
    python3 scripts/pricing/snapshot.py --check    # fail if a price is missing in us-east-1

Run it when AWS changes a price or a family needs a new one, and commit the
result. Only the standard library is used.
"""

import argparse
import datetime
import json
import pathlib
import re
import sys
import urllib.request

BASE = "https://pricing.us-east-1.amazonaws.com"
OUT = pathlib.Path(__file__).resolve().parents[2] / "internal" / "pricing" / "snapshot.json"

# Each price: the offer (service code), the usage type without its region
# prefix (us-east-1 has none, other regions add one such as "EUW1-"; a list
# when regions name it differently), any
# product attributes that must match, the unit Stratus bills it in and a
# description in AWS's terms.
PRICES = {
    "sqs-standard-requests": {
        "offer": "AWSQueueService", "usagetype": ["Requests-RBP", "Requests-Tier1"],
        "unit": "requests", "description": "SQS standard queue requests (each 64 KiB chunk of a payload is one request)",
    },
    "sqs-fifo-requests": {
        "offer": "AWSQueueService", "usagetype": ["Requests-FIFO-RBP", "Requests-FIFO-Tier1"],
        "unit": "requests", "description": "SQS FIFO queue requests (each 64 KiB chunk of a payload is one request)",
    },
    "sns-requests": {
        "offer": "AmazonSNS", "usagetype": "Requests-Tier1", "attributes": {"group": "SNS-Requests-Tier1"},
        "unit": "requests", "description": "SNS standard topic API requests, including Publish (each 64 KiB chunk is one request)",
    },
    "eventbridge-custom-events": {
        "offer": "AWSEvents", "usagetype": "Event-64K-Chunks", "attributes": {"operation": "PutEvents"},
        "unit": "events (64 KiB chunks)", "description": "EventBridge custom events published (each 64 KiB chunk is one event)",
    },
    "kinesis-shard-hours": {
        "offer": "AmazonKinesis", "usagetype": "Storage-ShardHour",
        "unit": "shard-hours", "description": "Kinesis Data Streams provisioned shard-hours",
    },
    "kinesis-put-payload-units": {
        "offer": "AmazonKinesis", "usagetype": "PutRequestPayloadUnits",
        "unit": "payload units (25 KiB)", "description": "Kinesis Data Streams PUT payload units (25 KiB each)",
    },
    "kinesis-extended-retention-shard-hours": {
        "offer": "AmazonKinesis", "usagetype": "Extended-ShardHour",
        "unit": "shard-hours", "description": "Kinesis Data Streams retention beyond 24 hours, up to 7 days",
    },
    "kinesis-long-term-retention": {
        "offer": "AmazonKinesis", "usagetype": "LongTermRetention-ByteHrs",
        "unit": "GB-months", "description": "Kinesis Data Streams data stored beyond 7 days",
    },
    "kinesis-efo-consumer-shard-hours": {
        "offer": "AmazonKinesis", "usagetype": "EnhancedFanoutHour",
        "unit": "consumer-shard-hours", "description": "Kinesis Data Streams enhanced fan-out consumer-shard-hours",
    },
    "kinesis-efo-data-retrieved": {
        "offer": "AmazonKinesis", "usagetype": "ReadBytes",
        "unit": "GB", "description": "Kinesis Data Streams data read through enhanced fan-out",
    },
    "msk-broker-hours-m7g-large": {
        "offer": "AmazonMSK", "usagetype": "Kafka.m7g.large", "attributes": {"group": "Broker"},
        "unit": "broker-hours", "description": "MSK provisioned broker-hours, kafka.m7g.large",
    },
    "msk-storage": {
        "offer": "AmazonMSK", "usagetype": "Kafka.Storage.GP2",
        "unit": "GB-months", "description": "MSK provisioned broker storage",
    },
}

REGION_PREFIX = re.compile(r"^[A-Z]{2,4}\d?-")


def fetch(path):
    with urllib.request.urlopen(BASE + path, timeout=120) as resp:
        return json.load(resp)


def base_usagetype(usagetype):
    return REGION_PREFIX.sub("", usagetype, count=1)


def tiers_for(offer, spec):
    """The price tiers of one price in one region's offer file, or None."""
    found = []
    for sku, product in offer["products"].items():
        attrs = product.get("attributes", {})
        wanted = spec["usagetype"] if isinstance(spec["usagetype"], list) else [spec["usagetype"]]
        if base_usagetype(attrs.get("usagetype", "")) not in wanted:
            continue
        if any(attrs.get(k) != v for k, v in spec.get("attributes", {}).items()):
            continue
        for term in offer["terms"].get("OnDemand", {}).get(sku, {}).values():
            tiers = []
            for dim in term["priceDimensions"].values():
                end = dim.get("endRange", "Inf")
                tiers.append({
                    "from": float(dim.get("beginRange", "0")),
                    "to": 0 if end == "Inf" else float(end),
                    "usd": float(dim["pricePerUnit"]["USD"]),
                })
            tiers.sort(key=lambda t: t["from"])
            if tiers not in found:
                found.append(tiers)
    if not found:
        return None
    if len(found) > 1:
        raise SystemExit(f"{spec['offer']} {spec['usagetype']}: {len(found)} different prices in one region; narrow the match")
    return found[0]


def build():
    snapshot = {
        "source": BASE + "/offers/v1.0/aws/",
        "fetched": datetime.date.today().isoformat(),
        "published": "",
        "prices": {},
    }
    published = []
    for offer_code in sorted({p["offer"] for p in PRICES.values()}):
        index = fetch(f"/offers/v1.0/aws/{offer_code}/current/region_index.json")
        for region in sorted(index["regions"]):
            offer = fetch(index["regions"][region]["currentVersionUrl"])
            published.append(offer["publicationDate"][:10])
            for price_id, spec in PRICES.items():
                if spec["offer"] != offer_code:
                    continue
                tiers = tiers_for(offer, spec)
                if tiers is None:
                    continue
                entry = snapshot["prices"].setdefault(price_id, {
                    "service": offer_code, "unit": spec["unit"], "description": spec["description"], "regions": {},
                })
                entry["regions"][region] = tiers
            print(f"{offer_code} {region}", file=sys.stderr)
    snapshot["published"] = max(published)
    return snapshot


def check(snapshot):
    missing = [p for p in PRICES if "us-east-1" not in snapshot["prices"].get(p, {}).get("regions", {})]
    if missing:
        raise SystemExit("missing in us-east-1: " + ", ".join(missing))


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--check", action="store_true", help="only check the committed snapshot")
    args = ap.parse_args()
    if args.check:
        check(json.loads(OUT.read_text()))
        return
    snapshot = build()
    check(snapshot)
    OUT.parent.mkdir(parents=True, exist_ok=True)
    OUT.write_text(json.dumps(snapshot, indent=1, sort_keys=True) + "\n")
    print(f"wrote {OUT} (published {snapshot['published']})", file=sys.stderr)


if __name__ == "__main__":
    main()
