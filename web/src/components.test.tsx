import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import {
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
