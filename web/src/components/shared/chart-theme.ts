import type { EChartsOption } from "echarts";

export function chartColors(dark: boolean) {
  return {
    ink: dark ? "#f2f1ed" : "#26251e",
    ash: dark ? "#b5b4ac" : "#7a7974",
    stone: dark ? "#3d3c36" : "#cdcdc9",
    bone: dark ? "#1c1b16" : "#f2f1ed",
    parchment: dark ? "#161511" : "#f7f7f4",
    ember: dark ? "#ff6a1a" : "#f54e00",
    forest: dark ? "#4a9d7a" : "#34785c",
    amber: dark ? "#d4a04a" : "#c08532",
    crimson: dark ? "#e85a7a" : "#cf2d56",
    verdant: dark ? "#3aa87d" : "#1f8a65",
  };
}

export function baseOption(dark: boolean): EChartsOption {
  const c = chartColors(dark);
  return {
    backgroundColor: "transparent",
    textStyle: { fontFamily: "Inter, sans-serif", color: c.ash, fontSize: 11 },
    grid: { left: 8, right: 8, top: 28, bottom: 8, containLabel: true },
    tooltip: {
      backgroundColor: c.bone,
      borderColor: c.stone,
      textStyle: { color: c.ink, fontSize: 12 },
    },
    legend: { textStyle: { color: c.ash }, icon: "rect", itemWidth: 8, itemHeight: 8 },
  };
}
