import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import { App } from './App'
import './styles.css'

// basename matches where the gateway mounts the app, so a client-side route is
// /ui/keys rather than /keys — which would 404 on reload against the gateway's
// own mux.
createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <BrowserRouter basename="/ui">
      <App />
    </BrowserRouter>
  </StrictMode>,
)
