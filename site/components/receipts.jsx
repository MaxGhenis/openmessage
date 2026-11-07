import { IconChecks } from "@tabler/icons-react";

import { RECEIPTS, receiptLabel, receiptUrl } from "../data/facts";

// A read receipt for a claim: a link to the line of code behind it, pinned to
// the release tag (or commit) the claim was checked against. tests/facts.test.js
// fails if a cited line stops saying what the receipt expects.
export function Receipts({ ids }) {
  return (
    <p className="receipts">
      {ids.map((id) => {
        const receipt = RECEIPTS[id];
        return (
          <a key={id} className="receipt" href={receiptUrl(receipt)} rel="noopener">
            <IconChecks stroke={2} aria-hidden="true" />
            <span className="visually-hidden">Source: </span>
            {receiptLabel(receipt)}
          </a>
        );
      })}
    </p>
  );
}
