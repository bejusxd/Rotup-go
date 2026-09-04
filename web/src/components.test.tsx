import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import {
  bytes,
  duration,
  ErrorAlert,
  StatusBadge,
  SystemContext,
  TechnicalDetails,
} from "./components";
import { RequestError } from "./api/client";
import type { System } from "./api/types";

describe("operations presentation", () => {
  it("renders human-readable status without using color alone", () => {
    render(
      <>
        <StatusBadge value="STABLE" />
        <StatusBadge value="RECOVERY_REQUIRED" />
        <StatusBadge value="NEW_UNKNOWN_STATE" />
      </>,
    );
    expect(screen.getByText("Ready")).toBeVisible();
    expect(screen.getByText("Needs attention")).toBeVisible();
    expect(screen.getByText("New unknown state")).toBeVisible();
  });
  it("shows friendly errors with exact backend evidence in technical details", () => {
    render(
      <ErrorAlert
        error={
          new RequestError(
            {
              code: "wrong_uuid",
              class: "INTEGRITY",
              message: "UUID differs from registered filesystem.",
              userMessage: "This disk cannot be used safely.",
            },
            409,
          )
        }
      />,
    );
    expect(screen.getByRole("alert")).toHaveTextContent(
      "This disk cannot be used safely.",
    );
    expect(screen.getByText("INTEGRITY / wrong_uuid")).toBeInTheDocument();
    expect(
      screen.getByText("UUID differs from registered filesystem."),
    ).toBeInTheDocument();
  });
  it.each([true, false])(
    "opens technical details only in dev mode %s",
    (devMode) => {
      render(
        <SystemContext.Provider value={{ devMode } as System}>
          <TechnicalDetails>state-version-42</TechnicalDetails>
        </SystemContext.Provider>,
      );
      expect(
        screen
          .getByText("Technical details")
          .parentElement!.hasAttribute("open"),
      ).toBe(devMode);
      expect(screen.getByText("state-version-42")).toBeInTheDocument();
    },
  );
});

describe("operator-readable magnitudes", () => {
  it("scales a disk capacity to the unit an operator can compare", () => {
    // The platform reports bytes; every size used to be printed in gigabytes,
    // so a 374 MB volume and a 167 MB volume both read as "0.x GB".
    expect(bytes(374251520)).toMatch(/^374\s*MB$/);
    expect(bytes(167149568)).toMatch(/^167\s*MB$/);
    expect(bytes(1099511627776)).toMatch(/^1[.,]1\s*TB$/);
    expect(bytes(0)).toBe("Not available");
  });
  it("keeps a long run duration readable instead of printing raw seconds", () => {
    expect(duration(139)).toBe("139 ms");
    expect(duration(8845)).toBe("8.8 s");
    expect(duration(134000)).toBe("2 min 14 s");
    expect(duration(7203400)).toBe("2 h 0 min");
  });
});
