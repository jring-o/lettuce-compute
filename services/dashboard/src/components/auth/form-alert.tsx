interface FormAlertProps {
  message: string;
}

export function FormAlert({ message }: FormAlertProps) {
  if (!message) return null;

  return (
    <div className="rounded-md border border-destructive/50 bg-destructive/10 px-4 py-3 text-sm text-destructive">
      {message}
    </div>
  );
}
