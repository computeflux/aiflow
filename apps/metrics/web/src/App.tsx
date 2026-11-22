import { BrowserRouter, Routes, Route } from "react-router-dom";
import { Sidebar } from "./components/Sidebar";
import { Overview } from "./pages/Overview";
import { Status } from "./pages/Status";
import { Config } from "./pages/Config";
import { Hosts } from "./pages/Hosts";

export default function App() {
  return (
    <BrowserRouter>
      <div className="flex h-screen text-[#FAFAFA] overflow-hidden">
        <Sidebar />
        <Routes>
          <Route path="/" element={<Overview />} />
          <Route path="/certs" element={<Status />} />
          <Route path="/hosts" element={<Hosts />} />
          <Route path="/config" element={<Config />} />
        </Routes>
      </div>
    </BrowserRouter>
  );
}
