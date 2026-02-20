import type { Metadata } from "next";
import "./globals.css";
import { Nav } from "../components/Nav";
import { AppShell } from "../components/ui/AppShell";

export const metadata: Metadata = {
  title: "LangOpen Portal",
  description: "Kubernetes-native LangGraph-compatible control portal"
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body>
        <AppShell nav={<Nav />}>{children}</AppShell>
      </body>
    </html>
  );
}
