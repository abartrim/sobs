from unittest.mock import patch

from sobs.__main__ import main


def test_main_delegates_to_app_module() -> None:
    with patch("runpy.run_module") as run_module:
        main()

    run_module.assert_called_once_with("app", run_name="__main__")
