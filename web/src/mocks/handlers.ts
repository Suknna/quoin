import { http, HttpResponse, type RequestHandler } from 'msw'
import { domainHandlers } from './domain/handlers'

export { DEMO_CREDENTIALS, getMockScenario, resetMockState, setMockScenario, type MockScenario } from './domain/store'

/** Install this ordered list with setupWorker/setupServer. The final handler makes missing local contracts visible. */
export const handlers: RequestHandler[] = [
  ...domainHandlers,
  http.all('*/api/*', ({ request }) => HttpResponse.json({
    message: `Mock API has no handler for ${request.method} ${new URL(request.url).pathname}.`,
    detail: 'This offline demo does not emulate unsupported backend or remote-browser operations.',
    code: 'mock_handler_missing',
  }, { status: 501 })),
]
