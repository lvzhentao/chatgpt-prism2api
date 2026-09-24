import { Component, type ReactNode } from "react";
import { Button } from "@/components/ui/button";

export class ErrorBoundary extends Component<{ children: ReactNode }, { err?: Error }> {
  state: { err?: Error } = {};
  static getDerivedStateFromError(err: Error) {
    return { err };
  }
  render() {
    if (!this.state.err) return this.props.children;
    return (
      <div className="mx-auto max-w-lg px-4 py-16">
        <h1 className="text-[26px] tracking-[-0.312px] text-ink">页面出错了</h1>
        <p className="mt-2 font-serif text-[16px] text-driftwood">{this.state.err.message}</p>
        <Button className="mt-6" onClick={() => this.setState({ err: undefined })}>
          重试
        </Button>
      </div>
    );
  }
}
