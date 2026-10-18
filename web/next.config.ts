import type { NextConfig } from "next";

// A static export: the site is HTML, JS and the JSON the benchmarks wrote,
// so it can be hosted anywhere. The console talks to a local `thaw serve`
// from the browser when one is running.
const nextConfig: NextConfig = {
  output: "export",
  images: { unoptimized: true },
};

export default nextConfig;
