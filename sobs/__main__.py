import runpy


def main() -> None:
    runpy.run_module("app", run_name="__main__")


if __name__ == "__main__":
    main()
