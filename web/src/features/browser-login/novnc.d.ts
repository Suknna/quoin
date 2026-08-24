declare module '@novnc/novnc' {
  export default class RFB {
    constructor(target: HTMLElement, url: string, options?: { shared?: boolean })
    scaleViewport: boolean
    resizeSession: boolean
    viewOnly: boolean
    disconnect(): void
    focus(options?: FocusOptions): void
    addEventListener(type: 'connect' | 'disconnect', listener: (event: Event) => void): void
  }
}
