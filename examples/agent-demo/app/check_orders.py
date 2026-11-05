"""The test the agent runs: place 25 orders and read them back."""

import sys

import orders


def main() -> int:
    ddb = orders.client()
    for i in range(25):
        orders.place_order(ddb, f"order-{i}", 1000 + i)
    n = orders.count_orders(ddb)
    if n != 25:
        print(f"FAIL: expected 25 orders, found {n}")
        return 1
    print("PASS: 25 orders placed and read back")
    return 0


if __name__ == "__main__":
    sys.exit(main())
