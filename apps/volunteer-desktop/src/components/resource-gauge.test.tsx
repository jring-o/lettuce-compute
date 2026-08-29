import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { ResourceGauge } from "./resource-gauge";

describe("ResourceGauge", () => {
  it("renders the label and display value", () => {
    render(
      <ResourceGauge label="CPU" value={45} displayValue="45%" />
    );
    expect(screen.getByText("CPU")).toBeInTheDocument();
    expect(screen.getByText("45%")).toBeInTheDocument();
  });

  it("renders an SVG element", () => {
    const { container } = render(
      <ResourceGauge label="GPU" value={80} displayValue="80%" />
    );
    expect(container.querySelector("svg")).toBeInTheDocument();
  });

  it("renders a green arc for low values (below green threshold)", () => {
    const { container } = render(
      <ResourceGauge label="CPU" value={30} displayValue="30%" />
    );
    // Two path elements: background track + value arc
    const paths = container.querySelectorAll("path");
    expect(paths).toHaveLength(2);
    // Value arc should be green
    expect(paths[1].getAttribute("stroke")).toBe("hsl(142, 76%, 36%)");
  });

  it("renders a yellow arc for mid values (between green and yellow thresholds)", () => {
    const { container } = render(
      <ResourceGauge label="CPU" value={75} displayValue="75%" />
    );
    const paths = container.querySelectorAll("path");
    expect(paths).toHaveLength(2);
    expect(paths[1].getAttribute("stroke")).toBe("hsl(45, 93%, 47%)");
  });

  it("renders a red arc for high values (above yellow threshold)", () => {
    const { container } = render(
      <ResourceGauge label="CPU" value={95} displayValue="95%" />
    );
    const paths = container.querySelectorAll("path");
    expect(paths).toHaveLength(2);
    expect(paths[1].getAttribute("stroke")).toBe("hsl(0, 84%, 60%)");
  });

  it("renders only the background track when value is 0", () => {
    const { container } = render(
      <ResourceGauge label="CPU" value={0} displayValue="0%" />
    );
    // Only the background path, no value arc
    const paths = container.querySelectorAll("path");
    expect(paths).toHaveLength(1);
  });

  it("clamps value above 100 to 100", () => {
    const { container } = render(
      <ResourceGauge label="CPU" value={150} displayValue="150%" />
    );
    // Should still render a value arc (clamped to 100)
    const paths = container.querySelectorAll("path");
    expect(paths).toHaveLength(2);
    // Red at 100%
    expect(paths[1].getAttribute("stroke")).toBe("hsl(0, 84%, 60%)");
  });

  it("clamps negative value to 0", () => {
    const { container } = render(
      <ResourceGauge label="CPU" value={-10} displayValue="-10%" />
    );
    // Clamped to 0 means no value arc
    const paths = container.querySelectorAll("path");
    expect(paths).toHaveLength(1);
  });

  it("shows temperature when provided and > 0", () => {
    render(
      <ResourceGauge label="CPU" value={45} displayValue="45%" temperature={65} />
    );
    expect(screen.getByText("CPU 65°C")).toBeInTheDocument();
  });

  it("does not show temperature when 0", () => {
    render(
      <ResourceGauge label="GPU" value={0} displayValue="No GPU" temperature={0} />
    );
    expect(screen.queryByText(/°C/)).not.toBeInTheDocument();
  });

  it("does not show temperature when not provided", () => {
    render(
      <ResourceGauge label="CPU" value={45} displayValue="45%" />
    );
    expect(screen.queryByText(/°C/)).not.toBeInTheDocument();
  });

  it("applies red text class when temperature >= 85", () => {
    render(
      <ResourceGauge label="CPU" value={95} displayValue="95%" temperature={90} />
    );
    const tempSpan = screen.getByText("CPU 90°C");
    expect(tempSpan.className).toContain("text-red-500");
  });

  it("applies yellow text class when temperature is 70-84", () => {
    render(
      <ResourceGauge label="CPU" value={80} displayValue="80%" temperature={75} />
    );
    const tempSpan = screen.getByText("CPU 75°C");
    expect(tempSpan.className).toContain("text-yellow-500");
  });

  it("respects custom color thresholds", () => {
    const { container } = render(
      <ResourceGauge
        label="CPU"
        value={55}
        displayValue="55%"
        colorThresholds={{ green: 50, yellow: 80 }}
      />
    );
    const paths = container.querySelectorAll("path");
    // 55 >= green(50) but < yellow(80) = yellow
    expect(paths[1].getAttribute("stroke")).toBe("hsl(45, 93%, 47%)");
  });

  it("renders different SVG sizes", () => {
    const { container: smContainer } = render(
      <ResourceGauge label="CPU" value={50} displayValue="50%" size="sm" />
    );
    const { container: lgContainer } = render(
      <ResourceGauge label="CPU" value={50} displayValue="50%" size="lg" />
    );
    const smSvg = smContainer.querySelector("svg");
    const lgSvg = lgContainer.querySelector("svg");
    expect(smSvg?.getAttribute("width")).toBe("100");
    expect(lgSvg?.getAttribute("width")).toBe("160");
  });
});
