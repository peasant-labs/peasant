/**
 * A minimal WebSocket stand-in for tests that mount the REAL provider.
 *
 * `WebSocketProvider` reads the global `WebSocket`, so a suite that wants the
 * real `useChannel` (not a mock of it) installs this class for the test body.
 * The static ready-state constants matter: the provider compares against
 * `WebSocket.OPEN` and `WebSocket.CONNECTING` to decide whether it may send.
 */
export class MockWebSocket {
  static instances: MockWebSocket[] = [];

  static readonly CONNECTING = 0;
  static readonly OPEN = 1;
  static readonly CLOSING = 2;
  static readonly CLOSED = 3;

  onopen: ((ev: Event) => void) | null = null;
  onmessage: ((ev: MessageEvent) => void) | null = null;
  onclose: ((ev: Event) => void) | null = null;
  onerror: ((ev: Event) => void) | null = null;
  readyState: number = MockWebSocket.CONNECTING;
  sent: string[] = [];

  constructor(readonly url: string) {
    MockWebSocket.instances.push(this);
  }

  static reset(): void {
    MockWebSocket.instances = [];
  }

  static latest(): MockWebSocket {
    const socket = MockWebSocket.instances.at(-1);
    if (!socket) throw new Error('no MockWebSocket was created; mount WebSocketProvider first');
    return socket;
  }

  send(data: string): void {
    this.sent.push(data);
  }

  close(): void {
    this.readyState = MockWebSocket.CLOSED;
  }

  simulateOpen(): void {
    this.readyState = MockWebSocket.OPEN;
    this.onopen?.(new Event('open'));
  }

  simulateMessage(data: unknown): void {
    this.simulateRawMessage(JSON.stringify(data));
  }

  simulateRawMessage(data: string): void {
    this.onmessage?.(new MessageEvent('message', { data }));
  }

  simulateClose(): void {
    this.readyState = MockWebSocket.CLOSED;
    this.onclose?.(new Event('close'));
  }
}
