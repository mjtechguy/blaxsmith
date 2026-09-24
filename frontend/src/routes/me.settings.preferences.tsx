import { createFileRoute } from "@tanstack/react-router";
import { setPrefs, usePrefs, type DateStyle, type Theme } from "../preferences";
import { Card } from "../ui";

export const Route = createFileRoute("/me/settings/preferences")({ component: Preferences });

// Display preferences apply immediately and are kept in this browser only.
function Preferences() {
  const prefs = usePrefs();
  const example = new Date(Date.now() - 42 * 60_000);
  return <Card title="Preferences" description="Applied immediately and kept in this browser. They never change what you can see or do.">
    <div className="card-body pref-list">
      <fieldset className="pref-group"><legend>Theme</legend>
        {(["light", "dark", "system"] as Theme[]).map((t) => <label key={t} className="recipe-check"><input type="radio" name="pref-theme" checked={prefs.theme === t} onChange={() => setPrefs({ theme: t })} /> {t === "system" ? "Match my system" : t === "light" ? "Light" : "Dark (charcoal)"}</label>)}
      </fieldset>
      <fieldset className="pref-group"><legend>Table density</legend>
        {(["comfortable", "compact"] as const).map((d) => <label key={d} className="recipe-check"><input type="radio" name="pref-density" checked={prefs.density === d} onChange={() => setPrefs({ density: d })} /> {d === "compact" ? "Compact rows" : "Comfortable rows"}</label>)}
        <small className="form-hint">The default for tables you have not adjusted; each table’s own density toggle still wins.</small>
      </fieldset>
      <fieldset className="pref-group"><legend>Dates and times</legend>
        {(["relative", "absolute"] as DateStyle[]).map((d) => <label key={d} className="recipe-check"><input type="radio" name="pref-dates" checked={prefs.dateStyle === d} onChange={() => setPrefs({ dateStyle: d })} />
          {d === "relative" ? "Relative (42m ago)" : `Absolute (${example.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" })})`}</label>)}
        <small className="form-hint">The exact local and UTC time is always one click away.</small>
      </fieldset>
    </div>
  </Card>;
}
