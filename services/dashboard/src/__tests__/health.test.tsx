import { render, screen } from "@testing-library/react";
import HealthPage from "@/app/health/page";

jest.mock("@/lib/db", () => ({
  db: {
    execute: jest.fn().mockResolvedValue([{ "?column?": 1 }]),
  },
}));

describe("Health Page", () => {
  it("renders health status information", async () => {
    const page = await HealthPage();
    render(page);

    expect(screen.getByText("Platform Health")).toBeInTheDocument();
    expect(screen.getByText("Status:")).toBeInTheDocument();
    expect(screen.getByText("Database:")).toBeInTheDocument();
    expect(screen.getByText("Timestamp:")).toBeInTheDocument();
  });

  it("shows healthy status when database is connected", async () => {
    const page = await HealthPage();
    render(page);

    expect(screen.getByText("healthy")).toBeInTheDocument();
    expect(screen.getByText("connected")).toBeInTheDocument();
  });

  it("shows unhealthy status when database is disconnected", async () => {
    const { db } = jest.requireMock("@/lib/db");
    db.execute.mockRejectedValueOnce(new Error("Connection refused"));

    const page = await HealthPage();
    render(page);

    expect(screen.getByText("unhealthy")).toBeInTheDocument();
    expect(screen.getByText("disconnected")).toBeInTheDocument();
  });
});
