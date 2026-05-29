import { fireEvent, render, screen } from "@testing-library/react";
import type { AnchorHTMLAttributes, ReactNode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { Header } from "../Header";

const { logoutMock } = vi.hoisted(() => ({
  logoutMock: vi.fn(() => Promise.resolve()),
}));

type MockRouterState = {
  location: {
    pathname: string;
  };
};

type MockLinkProps = AnchorHTMLAttributes<HTMLAnchorElement> & {
  children: ReactNode;
  to: string;
};

vi.mock("@tanstack/react-router", () => ({
  Link: ({ children, to, ...props }: MockLinkProps) => (
    <a href={to} {...props}>
      {children}
    </a>
  ),
  useRouterState: ({ select }: { select: (state: MockRouterState) => string }) =>
    select({ location: { pathname: "/jobs" } }),
}));

vi.mock("@/lib/auth", () => ({
  logout: logoutMock,
}));

vi.mock("../../command-menu", () => ({
  CommandMenu: () => <button type="button">Search...</button>,
}));

vi.mock("../../mode-toggle", () => ({
  ModeToggle: () => <button type="button">Toggle theme</button>,
}));

describe("Header", () => {
  beforeEach(() => {
    logoutMock.mockClear();
  });

  it("calls logout from the sign-out control", () => {
    render(<Header />);

    const signOut = screen.getByRole("button", { name: "Sign out" });
    expect(signOut).toBeVisible();

    fireEvent.click(signOut);

    expect(logoutMock).toHaveBeenCalledTimes(1);
  });
});
