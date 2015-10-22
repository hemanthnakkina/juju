"""Testing helpers and base classes for better isolation."""

import logging
import os
import StringIO
import subprocess
import unittest

import utility


class TestCase(unittest.TestCase):
    """TestCase provides a better isolated version of unittest.TestCase."""

    log_level = logging.INFO

    def setUp(self):
        super(TestCase, self).setUp()

        def _must_not_Popen(*args, **kwargs):
            """Tests may patch Popen but should never call it."""
            self.fail("subprocess.Popen(*{!r}, **{!r}) called".format(
                args, kwargs))

        self.addCleanup(setattr, subprocess, "Popen", subprocess.Popen)
        subprocess.Popen = _must_not_Popen

        self.addCleanup(setattr, os, "environ", os.environ)
        os.environ = {}

        setup_test_logging(self, self.log_level)


class FakeHomeTestCase(TestCase):
    """FakeHomeTestCase creates an isolated home dir for Juju to use."""

    def setUp(self):
        super(FakeHomeTestCase, self).setUp()
        ctx = utility.temp_dir()
        self.home_dir = ctx.__enter__()
        self.addCleanup(ctx.__exit__, None, None, None)
        os.environ["HOME"] = self.home_dir
        os.environ["PATH"] = os.path.join(self.home_dir, ".local", "bin")
        os.mkdir(os.path.join(self.home_dir, ".juju"))


def setup_test_logging(testcase, level=None):
    log = logging.getLogger()
    testcase.addCleanup(setattr, log, 'handlers', log.handlers)
    log.handlers = []
    testcase.log_stream = StringIO.StringIO()
    handler = logging.StreamHandler(testcase.log_stream)
    handler.setFormatter(logging.Formatter("%(levelname)s %(message)s"))
    log.addHandler(handler)
    if level is not None:
        testcase.addCleanup(log.setLevel, log.level)
        log.setLevel(level)
