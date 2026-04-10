export function PageHeader({
  eyebrow,
  title,
  description,
}: {
  eyebrow: string;
  title: string;
  description: string;
}) {
  return (
    <div className="portal-frame relative overflow-hidden px-6 py-8 sm:px-8 sm:py-10">
      <div className="pointer-events-none absolute inset-y-0 right-0 w-72 bg-[radial-gradient(circle_at_center,rgba(151,197,255,0.18),transparent_68%)]" />
      <div className="relative flex flex-col gap-4">
        <p className="portal-kicker">{eyebrow}</p>
        <div className="flex flex-col gap-3">
          <h2 className="portal-display max-w-4xl text-5xl leading-[0.95] text-slate-950 sm:text-6xl">
            {title}
          </h2>
          <p className="max-w-3xl text-base leading-7 text-slate-600 sm:text-lg">
            {description}
          </p>
        </div>
      </div>
    </div>
  );
}
